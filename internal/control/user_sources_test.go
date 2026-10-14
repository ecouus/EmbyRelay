package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAdminUserSourcesPrivacyOwnershipAndAddresses(t *testing.T) {
	a, admin, owner, _ := subscriptionFixture(t)
	a.verifySource = func(context.Context, Upstream, embyCredentials) error { return nil }
	portal := portalLogin(t, a, "viewer")
	upstreams, _ := list[Upstream](a.Store, "upstream")
	sourceURL := upstreams[0].BaseURL
	create := func(cookie *http.Cookie, name string) {
		t.Helper()
		response := call(t, a, cookie, "POST", "/api/v1/portal/sources", map[string]string{"name": name, "base_url": sourceURL, "emby_username": "private-source-user", "emby_password": "private-source-password"})
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	create(portal, "My source")
	created := call(t, a, admin, "POST", "/api/v1/access-accounts", map[string]any{"plan_id": owner.PlanID, "username": "other", "password": "viewer-password", "start_mode": "immediate"})
	if created.Code != 201 {
		t.Fatal(created.Body.String())
	}
	create(portalLogin(t, a, "other"), "Other source")
	sources, _ := list[Upstream](a.Store, "upstream")
	var source Upstream
	for _, s := range sources {
		if s.Name == "My source" {
			source = s
		}
	}
	if source.EncryptedCredentials == "" {
		t.Fatal("source credentials not encrypted")
	}
	routes, _ := list[Route](a.Store, "route")
	var route Route
	for _, r := range routes {
		if r.UpstreamID == source.ID {
			route = r
		}
	}
	node := Node{ID: "relay-b", Name: "Relay B", Endpoint: "https://relay-b.example.com", Enabled: true, SubscriptionSupport: true, DeviceIdentitySupport: true, LastSeen: time.Now().Unix(), ConfigVersion: 1 << 60}
	if err := a.Store.put("node", node.ID, node, "test"); err != nil {
		t.Fatal(err)
	}
	route.RelayID = node.ID
	if err := a.Store.put("route", route.ID, route, "test"); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(); err != nil {
		t.Fatal(err)
	}
	keys, _ := list[Key](a.Store, "key")
	var key Key
	for _, k := range keys {
		if k.RouteID == route.ID && k.AccountID == owner.ID {
			key = k
		}
	}
	secret, err := a.unseal(key.EncryptedSecret)
	if err != nil {
		t.Fatal(err)
	}
	read := func(path string) []UserSourceSummary {
		t.Helper()
		w := call(t, a, admin, "GET", path, nil)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(w.Code, w.Body.String())
		}
		for _, value := range []string{"private-source-user", "private-source-password", source.EncryptedCredentials, owner.PasswordHash, "encrypted_credentials", "password_hash"} {
			if strings.Contains(w.Body.String(), value) {
				t.Fatalf("sensitive data exposed: %s", value)
			}
		}
		var out []UserSourceSummary
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := read("/api/v1/user-sources"); len(got) != 2 {
		t.Fatalf("admin/shared source included or user source missing: %v", got)
	}
	filtered := read("/api/v1/user-sources?account_id=" + owner.ID)
	if len(filtered) != 1 {
		t.Fatal(filtered)
	}
	result := filtered[0]
	if result.OwnerUsername != "viewer" || result.BaseURL != sourceURL || result.SourceUsernameMasked != "••••••••" || result.SourcePasswordMasked != "••••••••" || !result.HasCredentials {
		t.Fatal(result)
	}
	if len(result.Addresses) != 1 {
		t.Fatal(result.Addresses)
	}
	addr := result.Addresses[0]
	if addr.Origin != node.Endpoint || addr.Path != "/r/"+route.Alias+"/"+secret+"/" || !addr.Available {
		t.Fatal(addr)
	}
	portalList := call(t, a, portal, "GET", "/api/v1/portal/sources", nil)
	var ownSources []struct {
		ID          string              `json:"id"`
		Connections []UserSourceAddress `json:"connection_addresses"`
	}
	if err := json.Unmarshal(portalList.Body.Bytes(), &ownSources); err != nil {
		t.Fatal(err)
	}
	if len(ownSources) != 1 || len(ownSources[0].Connections) != 1 || ownSources[0].Connections[0].Origin != addr.Origin || ownSources[0].Connections[0].Path != addr.Path {
		t.Fatal("admin and portal addresses differ", portalList.Body.String())
	}
	// An unbound or foreign user's key must never be presented as this user's address.
	foreign := key
	foreign.ID = ID()
	foreign.AccountID = "other-account"
	a.Store.put("key", foreign.ID, foreign, "test")
	if len(read("/api/v1/user-sources?account_id=" + owner.ID)[0].Addresses) != 1 {
		t.Fatal("foreign key included")
	}
	route.RelayID = "auto"
	a.Store.put("route", route.ID, route, "test")
	addr = read("/api/v1/user-sources?account_id=" + owner.ID)[0].Addresses[0]
	if addr.Origin != "" || !strings.Contains(addr.EntryLabel, "自动选择") {
		t.Fatal("auto entry should use control origin", addr)
	}
	key.Enabled = false
	a.Store.put("key", key.ID, key, "test")
	addr = read("/api/v1/user-sources?account_id=" + owner.ID)[0].Addresses[0]
	if addr.Available || addr.UnavailableReason != "访问密钥已撤销" {
		t.Fatal(addr)
	}
	updated := call(t, a, portal, "PATCH", "/api/v1/portal/sources/"+source.ID, map[string]string{"name": "Updated source", "base_url": "https://updated.example.com"})
	if updated.Code != 200 {
		t.Fatal(updated.Body.String())
	}
	result = read("/api/v1/user-sources?account_id=" + owner.ID)[0]
	if result.Name != "Updated source" || result.BaseURL != "https://updated.example.com:443" {
		t.Fatal(result)
	}
	// The projection should not attempt credential decryption or return ciphertext.
	source, _ = get[Upstream](a.Store, "upstream", source.ID)
	source.EncryptedCredentials = "invalid-sealed-data"
	a.Store.put("upstream", source.ID, source, "test")
	result = read("/api/v1/user-sources?account_id=" + owner.ID)[0]
	if result.SourcePasswordMasked != "••••••••" {
		t.Fatal(result)
	}
	if w := call(t, a, nil, "GET", "/api/v1/user-sources", nil); w.Code != 401 {
		t.Fatal("anonymous access accepted", w.Code)
	}
	if w := call(t, a, portal, "GET", "/api/v1/user-sources", nil); w.Code != 401 {
		t.Fatal("subscriber access accepted", w.Code)
	}
	call(t, a, admin, "POST", "/api/v1/users", map[string]string{"username": "reader", "password": "operator-test-password", "role": "operator"})
	login := call(t, a, nil, "POST", "/api/v1/auth/login", map[string]string{"username": "reader", "password": "operator-test-password"})
	if login.Code != 200 {
		t.Fatal(login.Body.String())
	}
	if w := call(t, a, login.Result().Cookies()[0], "GET", "/api/v1/user-sources", nil); w.Code != 403 {
		t.Fatal("operator access accepted", w.Code)
	}
	if w := call(t, a, admin, "POST", "/api/v1/user-sources", map[string]any{}); w.Code != 405 {
		t.Fatal(w.Code)
	}
	deleted := call(t, a, portal, "DELETE", "/api/v1/portal/sources/"+source.ID, nil)
	if deleted.Code != 200 {
		t.Fatal(deleted.Body.String())
	}
	if len(read("/api/v1/user-sources?account_id="+owner.ID)) != 0 {
		t.Fatal("deleted source still listed")
	}
}
