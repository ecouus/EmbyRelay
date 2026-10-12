package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func portalLogin(t *testing.T, a *App, name string) *http.Cookie {
	t.Helper()
	w := call(t, a, nil, "POST", "/api/v1/portal/login", map[string]string{"username": name, "password": "viewer-password"})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "relay_portal" {
			return c
		}
	}
	t.Fatal("missing portal cookie")
	return nil
}
func TestPortalSharedAccountIsolation(t *testing.T) {
	a, admin, c, _ := subscriptionFixture(t)
	pc := portalLogin(t, a, "viewer")
	sources, _ := list[Upstream](a.Store, "upstream")
	base := sources[0].BaseURL
	create := func(name string) string {
		t.Helper()
		w := call(t, a, pc, "POST", "/api/v1/portal/sources", map[string]string{"name": name, "base_url": base, "emby_username": "real-user", "emby_password": "real-password"})
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		us, _ := list[Upstream](a.Store, "upstream")
		for _, u := range us {
			if u.Name == name {
				return u.ID
			}
		}
		t.Fatal("source absent")
		return ""
	}
	first := create("one")
	second := create("two")
	w := call(t, a, pc, "GET", "/api/v1/portal/sources", nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "real-password") || strings.Contains(w.Body.String(), "encrypted_credentials") {
		t.Fatal(w.Body.String())
	}
	var listed []struct {
		ID        string   `json:"id"`
		Addresses []string `json:"addresses"`
	}
	json.Unmarshal(w.Body.Bytes(), &listed)
	if len(listed) != 2 {
		t.Fatal(w.Body.String())
	}
	// Only the administrator can permit a private source in this local integration fixture.
	for _, id := range []string{first, second} {
		u, _ := get[Upstream](a.Store, "upstream", id)
		u.AllowedCIDRs = []string{"127.0.0.1/32"}
		a.Store.put("upstream", id, u, "test")
	}
	a.Reload()
	tokens := map[string]string{}
	prefixes := map[string]string{}
	for _, item := range listed {
		prefix := strings.TrimSuffix(item.Addresses[0], "/")
		prefixes[item.ID] = prefix
		tokens[item.ID] = subscriptionLogin(t, a, prefix)
	}
	w = call(t, a, nil, "GET", prefixes[second]+"/Videos/1/stream?api_key="+tokens[first], nil)
	if w.Code != 403 {
		t.Fatal("cross source token accepted", w.Code)
	}
	keys, _ := list[Key](a.Store, "key")
	cmds := []AccessCommand{}
	for _, id := range []string{first, second} {
		rs, _ := list[Route](a.Store, "route")
		for _, r := range rs {
			if r.UpstreamID == id {
				for _, k := range keys {
					if k.RouteID == r.ID {
						cmds = append(cmds, AccessCommand{AccountID: c.ID, KeyID: k.ID, Token: tokens[id], Action: "begin", Stream: true})
					}
				}
			}
		}
	}
	one, e := a.accessCall(context.Background(), cmds[0])
	if e != nil {
		t.Fatal(e)
	}
	if _, e = a.accessCall(context.Background(), cmds[1]); e == nil {
		t.Fatal("shared concurrency bypass")
	}
	charge := cmds[0]
	charge.Lease = one.Lease
	charge.Action = "charge"
	charge.Bytes = 30000
	if _, e = a.accessCall(context.Background(), charge); e != nil {
		t.Fatal(e)
	}
	charge.Action = "finish"
	a.accessCall(context.Background(), charge)
	two, e := a.accessCall(context.Background(), cmds[1])
	if e != nil {
		t.Fatal(e)
	}
	charge = cmds[1]
	charge.Lease = two.Lease
	charge.Action = "charge"
	charge.Bytes = 25000
	if _, e = a.accessCall(context.Background(), charge); e == nil {
		t.Fatal("shared traffic bypass")
	}
	// Portal credentials never authorize administrator endpoints.
	if w = call(t, a, pc, "GET", "/api/v1/upstreams", nil); w.Code != 401 {
		t.Fatal("portal accessed administrator API")
	}
	w = call(t, a, admin, "POST", "/api/v1/access-accounts", map[string]any{"plan_id": c.PlanID, "username": "other", "password": "viewer-password", "start_mode": "immediate"})
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	other := portalLogin(t, a, "other")
	if w = call(t, a, other, "GET", "/api/v1/portal/sources", nil); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatal("other sources exposed", w.Body.String())
	}
	if w = call(t, a, other, "DELETE", "/api/v1/portal/sources/"+first, nil); w.Code == 200 {
		t.Fatal("cross user deletion allowed")
	}
	before, _ := get[AccessAccount](a.Store, "access_account", c.ID)
	if w = call(t, a, pc, "DELETE", "/api/v1/portal/sources/"+first, nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	create("replacement")
	after, _ := get[AccessAccount](a.Store, "access_account", c.ID)
	cycle, _ := accountCycle(after, time.Now())
	var used int64
	a.Store.DB.QueryRow("SELECT bytes FROM access_usage WHERE account_id=? AND cycle=?", c.ID, cycle).Scan(&used)
	if used != 60000 || before.ExpiresAt != after.ExpiresAt {
		t.Fatal("delete reset entitlement", used)
	}
	after.ExpiresAt = time.Now().Unix() - 1
	a.Store.put("access_account", c.ID, after, "test")
	if _, e = a.accessCall(context.Background(), cmds[1]); e == nil {
		t.Fatal("expired source still authorized")
	}
	if w = call(t, a, pc, "GET", "/api/v1/portal/me", nil); w.Code != 200 {
		t.Fatal("expired user cannot inspect subscription")
	}
	call(t, a, admin, "POST", "/api/v1/access-accounts/"+c.ID+"/status", map[string]bool{"enabled": false})
	if w = call(t, a, pc, "GET", "/api/v1/portal/me", nil); w.Code != 401 {
		t.Fatal("disabled portal session accepted")
	}
}

func TestPortalRejectsPolicyOverrides(t *testing.T) {
	a, _, _, _ := subscriptionFixture(t)
	cookie := portalLogin(t, a, "viewer")
	for _, field := range []string{"owner_account_id", "allowed_cidrs", "relay_id", "traffic_bytes"} {
		body := map[string]any{"name": "test", "base_url": "https://media.example.com", "emby_username": "demo", "emby_password": "demo-password", field: "override"}
		if w := call(t, a, cookie, "POST", "/api/v1/portal/sources", body); w.Code != 400 {
			t.Fatalf("accepted %s: %d", field, w.Code)
		}
	}
	w := call(t, a, cookie, "POST", "/api/v1/portal/sources", map[string]string{"name": "private", "base_url": "http://127.0.0.1:1", "emby_username": "demo", "emby_password": "demo-password"})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = call(t, a, cookie, "GET", "/api/v1/portal/sources", nil)
	var sources []struct {
		Addresses []string `json:"addresses"`
	}
	json.Unmarshal(w.Body.Bytes(), &sources)
	w = call(t, a, nil, "POST", sources[0].Addresses[0]+"Users/AuthenticateByName", map[string]string{"Username": "viewer", "Pw": "viewer-password"})
	if w.Code != 401 {
		t.Fatal("private source was reachable", w.Code)
	}
}

func TestUnifiedSubscriberLogin(t *testing.T) {
	a, _, _, _ := subscriptionFixture(t)
	for _, tc := range []struct {
		password string
		status   int
	}{{"wrong-password", 401}, {"viewer-password", 200}} {
		w := call(t, a, nil, "POST", "/api/v1/auth/login", map[string]string{"username": "viewer", "password": tc.password})
		if w.Code != tc.status {
			t.Fatalf("login status: %d, body: %s", w.Code, w.Body.String())
		}
		if tc.status == 200 {
			if !strings.Contains(w.Body.String(), `"role":"subscriber"`) {
				t.Fatal(w.Body.String())
			}
			found := false
			for _, cookie := range w.Result().Cookies() {
				if cookie.Name == "relay_portal" {
					found = true
				}
				if cookie.Name == "relay_session" && cookie.MaxAge >= 0 {
					t.Fatal("subscriber received management session")
				}
			}
			if !found {
				t.Fatal("missing subscriber session")
			}
		}
	}
}
