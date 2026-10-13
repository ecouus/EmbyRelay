package relay

import (
	"bytes"
	"context"
	"embyrelay/internal/control"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestAgentRecoversSameVersionAndCapabilityFilteredKeys(t *testing.T) {
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
	call("POST", "routes", control.Route{Alias: "test", UpstreamID: ups[0].ID, RelayID: agent.Identity.ID, Enabled: true, MaxConcurrent: 5, DailyQuota: 0})
	var routes []control.Route
	json.Unmarshal(call("GET", "routes", nil), &routes)
	var key map[string]string
	json.Unmarshal(call("POST", "routes/"+routes[0].ID+"/keys", map[string]int{"expires_at": 0}), &key)

	subSecret := "subscription-key"
	subKey := control.Key{ID: "subscription-key", RouteID: routes[0].ID, AccountID: "account", Hash: control.Hash(subSecret), Enabled: true}
	if _, e = s.DB.Exec("INSERT INTO objects VALUES('key',?,?,?)", subKey.ID, string(control.Encode(subKey)), time.Now().Unix()); e != nil {
		t.Fatal(e)
	}
	if e = app.Reload(); e != nil {
		t.Fatal(e)
	}
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

	subStatus := func() int {
		rr := httptest.NewRecorder()
		agentApp.ServeHTTP(rr, httptest.NewRequest("GET", "/r/test/"+subSecret+"/Users/Public", nil))
		return rr.Code
	}
	if subStatus() != 200 {
		t.Fatal("capability-specific same-version keys not applied")
	}
	savedVersion := agent.version
	address := server.Listener.Addr().String()
	server.Close()
	agent.validUntil = time.Now().Add(-time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	agent.Run(ctx)
	if play() != 404 {
		t.Fatal("expired offline cache still active")
	}
	listener, e := net.Listen("tcp", address)
	if e != nil {
		t.Fatal(e)
	}
	server = httptest.NewUnstartedServer(app)
	server.Listener = listener
	server.Start()
	defer server.Close()
	if e = agent.Sync(context.Background()); e != nil {
		t.Fatal(e)
	}
	if agent.version != savedVersion || play() != 200 || subStatus() != 200 {
		t.Fatal("same-version reconnect did not restore routes and keys")
	}
}

func TestSnapshotDigestIgnoresGenerationTime(t *testing.T) {
	a := control.Snapshot{Version: 5, GeneratedAt: 1, Routes: []control.Route{{ID: "route", Alias: "before"}}}
	b := a
	b.GeneratedAt = 2
	if snapshotDigest(a) != snapshotDigest(b) {
		t.Fatal("generation time changed effective config")
	}
	b.Routes = []control.Route{{ID: "route", Alias: "after"}}
	if snapshotDigest(a) == snapshotDigest(b) {
		t.Fatal("same-version content change was ignored")
	}
}
