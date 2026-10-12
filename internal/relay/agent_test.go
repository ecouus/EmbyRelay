package relay

import (
	"bytes"
	"context"
	"embyrelay/internal/control"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestAgentEnrollmentSyncOfflineAndQuota(t *testing.T) {
	master := bytes.Repeat([]byte{2}, 32)
	s, e := control.Open(filepath.Join(t.TempDir(), "control.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = s.Bootstrap("admin", "admin-test-password"); e != nil {
		t.Fatal(e)
	}
	app, e := control.New(s, master, false, nil)
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(app)
	defer server.Close()
	var cookie *http.Cookie
	call := func(method, path string, body any) []byte {
		t.Helper()
		r := httptest.NewRequest(method, "/api/v1/"+path, bytes.NewReader(control.Encode(body)))
		r.RemoteAddr = "127.0.0.1:1"
		r.Header.Set("X-Requested-With", "EmbyRelay")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		if w.Code < 200 || w.Code >= 300 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if path == "auth/login" {
			cookie = w.Result().Cookies()[0]
		}
		return w.Body.Bytes()
	}
	call("POST", "auth/login", map[string]string{"username": "admin", "password": "admin-test-password"})
	var enrolled map[string]any
	json.Unmarshal(call("POST", "relays/enroll", nil), &enrolled)
	dir := t.TempDir()
	as, e := control.Open(filepath.Join(dir, "agent.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer as.Close()
	agentApp, e := control.New(as, master, true, nil)
	if e != nil {
		t.Fatal(e)
	}
	agent, e := New(agentApp, as, dir, "http://127.0.0.1:8081", server.URL, "test-agent", enrolled["token"].(string), enrolled["server_key"].(string), true)
	if e != nil {
		t.Fatal(e)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("0123456789")) }))
	defer upstream.Close()
	call("POST", "upstreams", control.Upstream{Name: "test", BaseURL: upstream.URL, Enabled: true, Mode: "proxy", AllowedCIDRs: []string{"127.0.0.1/32"}})
	var ups []control.Upstream
	json.Unmarshal(call("GET", "upstreams", nil), &ups)
	call("POST", "routes", control.Route{Alias: "test", UpstreamID: ups[0].ID, RelayID: agent.Identity.ID, Enabled: true, MaxConcurrent: 5, DailyQuota: 100})
	var routes []control.Route
	json.Unmarshal(call("GET", "routes", nil), &routes)
	var key map[string]string
	json.Unmarshal(call("POST", "routes/"+routes[0].ID+"/keys", map[string]int{"expires_at": 0}), &key)
	if e = agent.Sync(context.Background()); e != nil {
		t.Fatal(e)
	}
	gateway := httptest.NewServer(agentApp)
	defer gateway.Close()
	play := func() int {
		t.Helper()
		resp, e := http.Get(gateway.URL + "/r/test/" + key["access_key"] + "/Videos/1/stream")
		if e != nil {
			t.Fatal(e)
		}
		defer resp.Body.Close()
		_, e = io.ReadAll(resp.Body)
		if e != nil {
			t.Fatal(e)
		}
		return resp.StatusCode
	}
	if play() != 200 {
		t.Fatal("agent playback failed")
	}
	traffic, _ := as.Traffic()
	if len(traffic) != 1 || traffic[0].Egress != 10 {
		t.Fatal("agent accounting mismatch", traffic)
	}
	if e = agent.Sync(context.Background()); e != nil {
		t.Fatal(e)
	}
	server.Close()
	if e = agent.Sync(context.Background()); e == nil {
		t.Fatal("expected disconnected control plane")
	}
	if play() != 200 {
		t.Fatal("cached configuration unavailable offline")
	}
	// A restart loads the signed snapshot and preserves spent and unspent quota.
	restartedApp, e := control.New(as, master, true, nil)
	if e != nil {
		t.Fatal(e)
	}
	restarted, e := New(restartedApp, as, dir, "", server.URL, "", "", "", true)
	if e != nil {
		t.Fatal(e)
	}
	if restarted.version != agent.version {
		t.Fatal("snapshot not restored")
	}
	restarted.validUntil = time.Now().Add(-time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	restarted.Run(ctx)
	rr := httptest.NewRecorder()
	restartedApp.ServeHTTP(rr, httptest.NewRequest("GET", "/r/test/"+key["access_key"]+"/Videos/1/stream", nil))
	if rr.Code != 404 {
		t.Fatal("expired cache remained active")
	}
	for i := 0; i < 8; i++ {
		if play() != 200 {
			t.Fatal("reserved budget not usable offline")
		}
	}
	if play() != 429 {
		t.Fatal("offline quota exceeded")
	}
}
