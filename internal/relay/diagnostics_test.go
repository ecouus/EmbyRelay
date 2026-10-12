package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"embyrelay/internal/control"
)

func TestSignedDiagnosticUploadRetryAndRestart(t *testing.T) {
	master := bytes.Repeat([]byte{5}, 32)
	store, err := control.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Bootstrap("admin", "admin-test-password"); err != nil {
		t.Fatal(err)
	}
	app, err := control.New(store, master, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	var loseAck atomic.Bool
	var rejectHeartbeat atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/heartbeat" && rejectHeartbeat.Load() {
			http.Error(w, "invalid report", 400)
			return
		}
		if r.URL.Path == "/api/v1/agent/diagnostics" && loseAck.Swap(false) {
			recorded := httptest.NewRecorder()
			app.ServeHTTP(recorded, r)
			if recorded.Code != 200 {
				t.Errorf("upload rejected: %s", recorded.Body.String())
			}
			http.Error(w, "temporary failure after commit", 503)
			return
		}
		app.ServeHTTP(w, r)
	}))
	defer server.Close()
	login := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"admin-test-password"}`))
	app.ServeHTTP(login, request)
	if login.Code != 200 {
		t.Fatal(login.Body.String())
	}
	cookie := login.Result().Cookies()[0]
	call := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, nil)
		r.AddCookie(cookie)
		r.Header.Set("X-Requested-With", "EmbyRelay")
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		return w
	}
	enrolled := call("/api/v1/relays/enroll")
	var enrollment map[string]any
	json.Unmarshal(enrolled.Body.Bytes(), &enrollment)
	dir := t.TempDir()
	agentStore, err := control.Open(filepath.Join(dir, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	agentApp, err := control.New(agentStore, master, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := New(agentApp, agentStore, dir, "http://127.0.0.1:8081", server.URL, "Relay B", enrollment["token"].(string), enrollment["server_key"].(string), true)
	if err != nil {
		t.Fatal(err)
	}
	d := control.Diagnostic{ID: control.ID(), Status: 401, Reason: "订阅：缺少代理登录 Token", CreatedAt: time.Now().Unix(), Method: "POST", Path: "/r/test/private-key//emby/Users/AuthenticateByName?api_key=private-token", Source: "local", Details: `{"code":"missing_token","unknown_token":"private-token","upstream_path":"/r/test/private-key/Users?api_key=private-token","target":"https://user:password@example.com/path?api_key=private-token"}`}
	if err = agentStore.RecordDiagnostic(d); err != nil {
		t.Fatal(err)
	}
	// A heartbeat failure must not prevent independent diagnostic upload.
	rejectHeartbeat.Store(true)
	if err = agent.Sync(context.Background()); err == nil {
		t.Fatal("heartbeat failure missing")
	} else {
		failure, ok := err.(*ControlHTTPError)
		if !ok || failure.Path != "/api/v1/agent/heartbeat" || failure.Status != 400 {
			t.Fatal(err)
		}
	}
	// Commit at A but lose the acknowledgement: B must retain and retry.
	loseAck.Store(true)
	if err = agent.UploadDiagnostics(context.Background()); err == nil {
		t.Fatal("lost acknowledgement accepted")
	}
	pending, err := agentStore.PendingDiagnostics()
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending: %v %v", pending, err)
	}
	agentStore.Close()
	agentStore, err = control.Open(filepath.Join(dir, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer agentStore.Close()
	agentApp, err = control.New(agentStore, master, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	agent, err = New(agentApp, agentStore, dir, "http://127.0.0.1:8081", server.URL, "Relay B", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if err = agent.UploadDiagnostics(context.Background()); err != nil {
		t.Fatal(err)
	}
	pending, err = agentStore.PendingDiagnostics()
	if err != nil || len(pending) != 0 {
		t.Fatalf("not acknowledged: %v %v", pending, err)
	}
	var count int
	store.DB.QueryRow("SELECT COUNT(*) FROM request_diagnostics WHERE node_id=?", agent.Identity.ID).Scan(&count)
	if count != 1 {
		t.Fatalf("duplicate records: %d", count)
	}
	r := httptest.NewRequest("GET", "/api/v1/diagnostics?node_id="+agent.Identity.ID, nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Relay B") {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, secret := range []string{"private-key", "private-token", "password", "unknown_token"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("diagnostics leaked %s", secret)
		}
	}
	r = httptest.NewRequest("GET", "/api/v1/diagnostics?node_id=local", nil)
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if strings.Contains(w.Body.String(), "Relay B") {
		t.Fatal("local filter included remote diagnostics")
	}
	// A rejects an unsigned upload even if the body contains plausible records.
	r = httptest.NewRequest("POST", "/api/v1/agent/diagnostics", bytes.NewReader(control.Encode(control.DiagnosticBatch{Records: []control.Diagnostic{d}})))
	w = httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("unsigned upload accepted: %d", w.Code)
	}
	// Run records a structured heartbeat failure without persisting raw response bodies.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	agent.Run(ctx)
	cancel()
	pending, err = agentStore.PendingDiagnostics()
	if err != nil || len(pending) != 1 || pending[0].Source != "agent" || pending[0].Path != "/api/v1/agent/heartbeat" || pending[0].Status != 400 {
		t.Fatalf("sync failure diagnostic: %v %v", pending, err)
	}
	if err = agent.UploadDiagnostics(context.Background()); err != nil {
		t.Fatal(err)
	}

}
