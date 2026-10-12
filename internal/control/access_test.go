package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func subscriptionFixture(t *testing.T) (*App, *http.Cookie, AccessAccount, string) {
	t.Helper()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/AuthenticateByName":
			var v map[string]string
			json.NewDecoder(r.Body).Decode(&v)
			if v["Username"] != "real-user" || v["Pw"] != "real-password" {
				t.Error("wrong source credentials")
			}
			writeJSON(w, 200, map[string]any{"AccessToken": "source-secret-token", "User": map[string]any{"Id": "source-user-id", "Name": "real-user", "Policy": map[string]bool{"IsAdministrator": false}}})
		case "/System/Info/Public", "/emby/System/Info/Public", "/emby/Branding/Configuration", "/emby/System/Ping":
			if r.Header.Get("X-Test-Require-Client") == "true" {
				fields := deviceRequestFields(r)
				if fields != (DeviceFields{ID: "phone", Name: "Phone", Client: "Lenna", Version: "1"}) {
					http.Error(w, "client metadata required", 403)
					return
				}
				if accessToken(r) != "" || r.Header.Get("Cookie") != "" {
					t.Error("authentication leaked into public discovery")
				}
			}
			writeJSON(w, 200, map[string]string{"ServerName": "Test", "Id": "test"})
		case "/emby/Items/1/Images/Primary":
			if (r.Header.Get("X-Emby-Token") != "" && r.Header.Get("X-Emby-Token") != "source-secret-token") || r.Header.Get("Cookie") != "" {
				t.Error("image authorization missing or proxy cookie leaked")
			}
			w.Header().Set("Content-Type", "image/png")
			if r.Header.Get("X-Emby-Token") == "" {
				w.Write([]byte("public-image"))
			} else {
				w.Write([]byte("test-image"))
			}
		case "/emby/Items/2/Images/Primary":
			if r.Header.Get("X-Emby-Token") != "source-secret-token" {
				http.Error(w, "private image", 401)
				return
			}
			w.Write([]byte("private-image"))
		case "/Videos/slow/stream":
			w.Write([]byte("start"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/Videos/1/stream":
			if r.Header.Get("X-Emby-Token") != "source-secret-token" {
				t.Error("source token missing")
			}
			w.Write(bytes.Repeat([]byte("v"), 1024))
		case "/Items/1/PlaybackInfo":
			if r.Header.Get("X-Emby-Token") != "source-secret-token" {
				t.Error("source token missing")
			}
			writeJSON(w, 200, map[string]any{"MediaSources": []any{map[string]string{"DirectStreamUrl": "/Videos/1/stream?api_key=source-secret-token"}}})
		default:
			writeJSON(w, 200, map[string]string{"Id": "source-user-id"})
		}
	}))
	t.Cleanup(source.Close)
	a, cookie := testApp(t)
	w := call(t, a, cookie, "POST", "/api/v1/upstreams", map[string]any{"name": "source", "base_url": source.URL, "allowed_cidrs": []string{"127.0.0.1/32"}, "emby_username": "real-user", "emby_password": "real-password"})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	us, _ := list[Upstream](a.Store, "upstream")
	w = call(t, a, cookie, "POST", "/api/v1/routes", map[string]any{"alias": "sub", "upstream_id": us[0].ID, "enabled": true, "max_concurrent": 100})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	rs, _ := list[Route](a.Store, "route")
	p := AccessPlan{Name: "30 days", DurationDays: 30, TrafficBytes: 100000, TrafficPeriod: "days", PeriodDays: 30, MaxConcurrent: 1}
	w = call(t, a, cookie, "POST", "/api/v1/access-plans", p)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	ps, _ := list[AccessPlan](a.Store, "access_plan")
	w = call(t, a, cookie, "POST", "/api/v1/access-accounts", map[string]any{"route_id": rs[0].ID, "plan_id": ps[0].ID, "username": "viewer", "password": "viewer-password", "start_mode": "first_login"})
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var result map[string]string
	json.Unmarshal(w.Body.Bytes(), &result)
	cs, _ := list[AccessAccount](a.Store, "access_account")
	return a, cookie, cs[0], "/r/sub/" + result["access_key"]
}

func TestDeleteSubscriptionAndUnusedPlan(t *testing.T) {
	a, admin, account, prefix := subscriptionFixture(t)
	blocked := call(t, a, admin, "DELETE", "/api/v1/access-plans/"+account.PlanID, nil)
	if blocked.Code != 400 {
		t.Fatalf("plan in use deletion status = %d", blocked.Code)
	}
	deleted := call(t, a, admin, "DELETE", "/api/v1/access-accounts/"+account.ID, nil)
	if deleted.Code != 200 {
		t.Fatalf("account deletion: %d %s", deleted.Code, deleted.Body.String())
	}
	if _, err := get[AccessAccount](a.Store, "access_account", account.ID); err == nil {
		t.Fatal("deleted account remains")
	}
	if result := call(t, a, nil, "GET", prefix+"/System/Ping", nil); result.Code != 401 {
		t.Fatalf("old key accepted: %d", result.Code)
	}
	deleted = call(t, a, admin, "DELETE", "/api/v1/access-plans/"+account.PlanID, nil)
	if deleted.Code != 200 {
		t.Fatalf("unused plan deletion: %d %s", deleted.Code, deleted.Body.String())
	}
}

func TestSubscriptionPublicDiscoveryPreservesClientWithoutToken(t *testing.T) {
	a, _, _, prefix := subscriptionFixture(t)
	for _, path := range []string{"/emby/System/Info/Public", "/emby/Branding/Configuration", "/emby/System/Ping"} {
		for _, header := range []string{"Authorization", "X-Emby-Authorization", "X-MediaBrowser-Authorization"} {
			t.Run(path+header, func(t *testing.T) {
				r := httptest.NewRequest("GET", prefix+path+"?api_key=stale-token", nil)
				r.Header.Set(header, deviceAuthorization(DeviceFields{ID: "phone", Name: "Phone", Client: "Lenna", Version: "1"}, "stale-token"))
				r.Header.Set("X-Emby-Token", "stale-token")
				r.Header.Set("Cookie", "untrusted=secret")
				r.Header.Set("X-Test-Require-Client", "true")
				w := httptest.NewRecorder()
				a.ServeHTTP(w, r)
				if w.Code != 200 {
					t.Fatalf("public discovery failed: %d %s", w.Code, w.Body.String())
				}
			})
		}
	}
}
func subscriptionLogin(t *testing.T, a *App, prefix string) string {
	t.Helper()
	w := call(t, a, nil, "POST", prefix+"/Users/AuthenticateByName", map[string]string{"Username": "viewer", "Pw": "viewer-password"})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "source-secret-token") || strings.Contains(w.Body.String(), "real-user") {
		t.Fatal("source identity leaked")
	}
	var data map[string]any
	json.Unmarshal(w.Body.Bytes(), &data)
	return data["AccessToken"].(string)
}
func TestSubscriptionPlaybackAndLifecycle(t *testing.T) {
	a, admin, c, prefix := subscriptionFixture(t)
	if c.StartedAt != 0 {
		t.Fatal("activated early")
	}
	w := call(t, a, nil, "GET", prefix+"/Videos/1/stream", nil)
	if w.Code != 401 {
		t.Fatal("anonymous playback allowed", w.Code)
	}
	w = call(t, a, nil, "POST", prefix+"/Users/AuthenticateByName", map[string]string{"Username": "viewer", "Pw": "wrong-pass"})
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	token := subscriptionLogin(t, a, prefix)
	c, _ = get[AccessAccount](a.Store, "access_account", c.ID)
	if c.StartedAt == 0 || c.ExpiresAt-c.StartedAt != 30*86400 {
		t.Fatal("activation incorrect")
	}
	for _, path := range []string{"/api/v1/access-accounts", "/api/v1/audit"} {
		w = call(t, a, admin, "GET", path, nil)
		if strings.Contains(w.Body.String(), "viewer-password") || strings.Contains(w.Body.String(), "real-password") || strings.Contains(w.Body.String(), c.PasswordHash) {
			t.Fatal("secret leaked")
		}
	}
	w = call(t, a, nil, "GET", prefix+"/Items/1/PlaybackInfo?api_key="+token, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "source-secret-token") {
		t.Fatal("token leak", w.Code, w.Body.String())
	}
	var data struct {
		MediaSources []struct {
			DirectStreamURL string `json:"DirectStreamUrl"`
		}
	}
	json.Unmarshal(w.Body.Bytes(), &data)
	if len(data.MediaSources) != 1 || !strings.Contains(data.MediaSources[0].DirectStreamURL, "/__access_media/") {
		t.Fatal(w.Body.String())
	}
	w = call(t, a, nil, "GET", data.MediaSources[0].DirectStreamURL, nil)
	if w.Code != 200 || w.Body.Len() != 1024 {
		t.Fatal("wrapped playback", w.Code, w.Body.String())
	}
	cycle, _ := accountCycle(c, time.Now())
	var used int64
	a.Store.DB.QueryRow("SELECT bytes FROM access_usage WHERE account_id=? AND cycle=?", c.ID, cycle).Scan(&used)
	if used < 2048 {
		t.Fatal("usage missing")
	}
	expire := c.ExpiresAt
	w = call(t, a, admin, "POST", "/api/v1/access-accounts/"+c.ID+"/renew", map[string]int{"days": 7})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	c, _ = get[AccessAccount](a.Store, "access_account", c.ID)
	if c.ExpiresAt != expire+7*86400 {
		t.Fatal("renewal anchor")
	}
	var after int64
	a.Store.DB.QueryRow("SELECT bytes FROM access_usage WHERE account_id=? AND cycle=?", c.ID, cycle).Scan(&after)
	if after != used {
		t.Fatal("renew reset usage")
	}
	call(t, a, admin, "POST", "/api/v1/access-accounts/"+c.ID+"/status", map[string]bool{"enabled": false})
	w = call(t, a, nil, "GET", prefix+"/Videos/1/stream?api_key="+token, nil)
	if w.Code != 403 {
		t.Fatal("disabled session allowed")
	}
}
func TestSubscriptionAtomicBudgetAndConcurrent(t *testing.T) {
	a, admin, c, prefix := subscriptionFixture(t)
	token := subscriptionLogin(t, a, prefix)
	cmd := AccessCommand{AccountID: c.ID, KeyID: c.KeyID, Token: token, Action: "begin", Stream: true}
	out, e := a.accessCall(context.Background(), cmd)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = a.accessCall(context.Background(), cmd); e == nil {
		t.Fatal("concurrent overrun")
	}
	cmd.Lease = out.Lease
	cmd.Action = "charge"
	cmd.Bytes = 49999
	if _, e = a.accessCall(context.Background(), cmd); e != nil {
		t.Fatal(e)
	}
	cmd.Bytes = 2
	if _, e = a.accessCall(context.Background(), cmd); e == nil {
		t.Fatal("quota exceeded")
	}
	cmd.Bytes = 1
	if _, e = a.accessCall(context.Background(), cmd); e != nil {
		t.Fatal("last available byte rejected", e)
	}
	if _, e = a.accessCall(context.Background(), cmd); e == nil {
		t.Fatal("exhausted quota allowed another byte")
	}
	cmd.Action = "check"
	cmd.Bytes = 0
	if _, e = a.accessCall(context.Background(), cmd); e != nil {
		t.Fatal("already charged final output must remain authorized", e)
	}
	call(t, a, admin, "POST", "/api/v1/access-accounts/"+c.ID+"/revoke-sessions", map[string]any{})
	if _, e = a.accessCall(context.Background(), cmd); e == nil {
		t.Fatal("revoked stream retained access")
	}
}

func TestSubscriptionFinishRefundsPartialChunkOnce(t *testing.T) {
	a, _, c, prefix := subscriptionFixture(t)
	token := subscriptionLogin(t, a, prefix)
	c, _ = get[AccessAccount](a.Store, "access_account", c.ID)
	cmd := AccessCommand{AccountID: c.ID, KeyID: c.KeyID, Token: token, Action: "begin", Stream: true}
	out, err := a.accessCall(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Lease = out.Lease
	cmd.Action = "charge"
	cmd.Bytes = 32 * 1024
	if _, err = a.accessCall(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	cmd.Bytes = streamAccessChunk + 1
	if _, err = a.accessCall(context.Background(), cmd); err == nil || err.Error() != "invalid charge" {
		t.Fatalf("oversized charge: %v", err)
	}
	cmd.Bytes = streamAccessChunk
	if _, err = a.accessCall(context.Background(), cmd); err == nil || err.Error() != "本周期剩余流量不足" {
		t.Fatalf("charge boundary: %v", err)
	}
	cmd.Action = "finish"
	cmd.RefundBytes = streamAccessChunk + 1
	if _, err = a.accessCall(context.Background(), cmd); err == nil || err.Error() != "invalid refund" {
		t.Fatalf("oversized refund: %v", err)
	}
	cmd.RefundBytes = 16 * 1024
	if _, err = a.accessCall(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	if _, err = a.accessCall(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	cycle, _ := accountCycle(c, time.Now())
	var used int64
	if err = a.Store.DB.QueryRow("SELECT bytes FROM access_usage WHERE account_id=? AND cycle=?", c.ID, cycle).Scan(&used); err != nil || used != 32*1024 {
		t.Fatalf("refunded usage = %d, err = %v", used, err)
	}
}
func TestSubscriptionCalendar(t *testing.T) {
	jan := time.Date(2027, 1, 31, 12, 0, 0, 0, time.UTC)
	if got := addMonths(jan, 1); got.Month() != time.February || got.Day() != 28 {
		t.Fatal(got)
	}
	c := AccessAccount{StartedAt: jan.Unix(), Plan: AccessPlan{TrafficPeriod: "days", PeriodDays: 30}}
	first, _ := accountCycle(c, jan)
	second, _ := accountCycle(c, jan.Add(30*24*time.Hour))
	if first == second {
		t.Fatal("cycle not rolled")
	}
	c.Plan.TrafficPeriod = "month"
	_, next := accountCycle(c, jan)
	if next != time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC).Unix() {
		t.Fatal(next)
	}
}
func TestSubscriptionAgentAuthorizations(t *testing.T) {
	a, _, c, prefix := subscriptionFixture(t)
	token := subscriptionLogin(t, a, prefix)
	cmd := AccessCommand{AccountID: c.ID, KeyID: c.KeyID, Token: token, Action: "begin"}
	if _, err := a.accessCommand(context.Background(), "unassigned-node", cmd); err == nil {
		t.Fatal("unassigned node authorized")
	}
	route, _ := get[Route](a.Store, "route", c.RouteID)
	route.RelayID = "node-a"
	a.Store.put("route", route.ID, route, "test")
	a.Reload()
	out, err := a.accessCommand(context.Background(), "node-a", cmd)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Lease = out.Lease
	cmd.Action = "charge"
	cmd.Bytes = 1
	if _, err = a.accessCommand(context.Background(), "node-b", cmd); err == nil {
		t.Fatal("lease accepted on wrong node")
	}
	if _, err = a.accessCommand(context.Background(), "node-a", cmd); err != nil {
		t.Fatal(err)
	}
}
func TestSubscriptionOngoingExpiry(t *testing.T) {
	a, _, c, prefix := subscriptionFixture(t)
	token := subscriptionLogin(t, a, prefix)
	cmd := AccessCommand{AccountID: c.ID, KeyID: c.KeyID, Token: token, Action: "begin", Stream: true}
	out, e := a.accessCall(context.Background(), cmd)
	if e != nil {
		t.Fatal(e)
	}
	c, _ = get[AccessAccount](a.Store, "access_account", c.ID)
	c.ExpiresAt = time.Now().Unix() - 1
	a.Store.put("access_account", c.ID, c, "test")
	cmd.Lease = out.Lease
	cmd.Action = "charge"
	cmd.Bytes = 1
	if _, e = a.accessCall(context.Background(), cmd); e == nil {
		t.Fatal("expired ongoing stream allowed")
	}
}

func TestSubscriptionRateAndReset(t *testing.T) {
	a, admin, c, prefix := subscriptionFixture(t)
	token := subscriptionLogin(t, a, prefix)
	c, _ = get[AccessAccount](a.Store, "access_account", c.ID)
	c.Plan.RateBPS = 1024
	a.Store.put("access_account", c.ID, c, "test")
	cmd := AccessCommand{AccountID: c.ID, KeyID: c.KeyID, Token: token, Action: "begin"}
	out, e := a.accessCall(context.Background(), cmd)
	if e != nil {
		t.Fatal(e)
	}
	cmd.Lease = out.Lease
	cmd.Action = "charge"
	cmd.Bytes = 1024
	one, e := a.accessCall(context.Background(), cmd)
	if e != nil || one.WaitNS < 900000000 {
		t.Fatal(one, e)
	}
	two, e := a.accessCall(context.Background(), cmd)
	if e != nil || two.WaitNS < 1800000000 {
		t.Fatal(two, e)
	}
	beforeCycle, _ := accountCycle(c, time.Now())
	w := call(t, a, admin, "POST", "/api/v1/access-accounts/"+c.ID+"/reset-usage", map[string]any{})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	c, _ = get[AccessAccount](a.Store, "access_account", c.ID)
	newCycle, _ := accountCycle(c, time.Now())
	if beforeCycle == newCycle {
		t.Fatal("manual reset lost")
	}
	var old int64
	a.Store.DB.QueryRow("SELECT bytes FROM access_usage WHERE account_id=? AND cycle=?", c.ID, beforeCycle).Scan(&old)
	if old != 4096 {
		t.Fatal("historical accounting changed", old)
	}
}

func TestSubscriptionSignedAgentRPC(t *testing.T) {
	a, _, c, prefix := subscriptionFixture(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	node := Node{DeviceIdentitySupport: true, SubscriptionSupport: true, ID: "signed-node", Name: "test", Endpoint: "https://node.example", PublicKey: base64.RawStdEncoding.EncodeToString(pub), Enabled: true}
	a.Store.put("node", node.ID, node, "test")
	route, _ := get[Route](a.Store, "route", c.RouteID)
	route.RelayID = node.ID
	a.Store.put("route", route.ID, route, "test")
	a.Reload()
	invoke := func(cmd AccessCommand) AccessResult {
		t.Helper()
		body := Encode(cmd)
		req := httptest.NewRequest("POST", "/api/v1/agent/access", bytes.NewReader(body))
		SignRequest(req, body, node.ID, priv)
		w := httptest.NewRecorder()
		a.ServeHTTP(w, req)
		var out AccessResult
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return out
	}
	_ = prefix
	result := invoke(AccessCommand{Action: "login", AccountID: c.ID, KeyID: c.KeyID, Username: "viewer", Password: "viewer-password", Device: "agent-phone", DeviceName: "Agent Phone", ClientName: "RealClient", ClientVersion: "4.0", ClientIP: "127.0.0.2"})
	if result.Error != "" || result.Token == "" {
		t.Fatal(result.Error)
	}
	cmd := AccessCommand{Action: "begin", AccountID: c.ID, KeyID: c.KeyID, Token: result.Token, Stream: true}
	started := invoke(cmd)
	if started.Error != "" || started.SourceToken != "source-secret-token" || started.DeviceIdentity == nil || started.DeviceIdentity.Original.ID != "agent-phone" {
		t.Fatal(started)
	}
	cmd.Action = "charge"
	cmd.Lease = started.Lease
	cmd.Bytes = 1024
	if out := invoke(cmd); out.Error != "" {
		t.Fatal(out.Error)
	}
	node.DeviceIdentitySupport = false
	a.Store.put("node", node.ID, node, "test")
	if out := invoke(cmd); out.Error == "" {
		t.Fatal("legacy agent accepted identity-aware session")
	}
}

func TestSubscriptionWatchdogStopsActiveStream(t *testing.T) {
	a, admin, c, prefix := subscriptionFixture(t)
	token := subscriptionLogin(t, a, prefix)
	relay := httptest.NewServer(a)
	defer relay.Close()
	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Get(relay.URL + prefix + "/Videos/slow/stream?api_key=" + token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	first := make([]byte, 5)
	if _, err = io.ReadFull(resp.Body, first); err != nil {
		t.Fatal(err)
	}
	w := call(t, a, admin, "POST", "/api/v1/access-accounts/"+c.ID+"/status", map[string]bool{"enabled": false})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	start := time.Now()
	io.ReadAll(resp.Body)
	if time.Since(start) > 9*time.Second {
		t.Fatal("active playback did not stop promptly")
	}
}

func TestSubscriptionCleanupRemovesRevokedTargets(t *testing.T) {
	a, admin, c, prefix := subscriptionFixture(t)
	token := subscriptionLogin(t, a, prefix)
	cmd := AccessCommand{AccountID: c.ID, KeyID: c.KeyID, Token: token, Action: "wrap", Target: "https://media.example/video"}
	out, err := a.accessCall(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	a.Store.Cleanup()
	var count int
	if err := a.Store.DB.QueryRow("SELECT COUNT(*) FROM access_targets WHERE id=?", out.Target).Scan(&count); err != nil || count != 1 {
		t.Fatal("live media target removed", count, err)
	}
	call(t, a, admin, "POST", "/api/v1/access-accounts/"+c.ID+"/revoke-sessions", map[string]any{})
	a.Store.Cleanup()
	if err := a.Store.DB.QueryRow("SELECT COUNT(*) FROM access_targets WHERE id=?", out.Target).Scan(&count); err != nil || count != 0 {
		t.Fatal("revoked media target retained", count, err)
	}
}

func TestSubscriptionLoginWithLeadingSlashes(t *testing.T) {
	a, _, _, prefix := subscriptionFixture(t)
	for _, path := range []string{"//emby/Users/AuthenticateByName", "///Users/AuthenticateByName", "/Emby/Users/AuthenticateByName"} {
		t.Run(path, func(t *testing.T) {
			bad := call(t, a, nil, "POST", prefix+path, map[string]string{"Username": "viewer", "Pw": "wrong-pass"})
			if bad.Code != 401 || !strings.Contains(bad.Body.String(), "用户名或密码错误") {
				t.Fatalf("wrong password bypassed login validation: %d %s", bad.Code, bad.Body.String())
			}
			login := call(t, a, nil, "POST", prefix+path, map[string]string{"Username": "viewer", "Pw": "viewer-password"})
			if login.Code != 200 {
				t.Fatalf("login: %d %s", login.Code, login.Body.String())
			}
			var data map[string]any
			if err := json.Unmarshal(login.Body.Bytes(), &data); err != nil {
				t.Fatal(err)
			}
			token, _ := data["AccessToken"].(string)
			if token == "" || token == "source-secret-token" || strings.Contains(login.Body.String(), "source-secret-token") {
				t.Fatal("missing proxy token or leaked source token")
			}
			request := httptest.NewRequest("GET", prefix+"//Videos/1/stream", nil)
			request.Header.Set("X-Emby-Token", token)
			response := httptest.NewRecorder()
			a.ServeHTTP(response, request)
			if response.Code != 200 || response.Body.Len() != 1024 {
				t.Fatalf("playback: %d %s", response.Code, response.Body.String())
			}
			anonymous := call(t, a, nil, "GET", prefix+"//Videos/1/stream", nil)
			if anonymous.Code != 401 {
				t.Fatalf("anonymous media accepted: %d", anonymous.Code)
			}
		})
	}
}
