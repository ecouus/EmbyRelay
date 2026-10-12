package control

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"embyrelay/internal/database"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testApp(t *testing.T) (*App, *http.Cookie) {
	t.Helper()
	s, e := testStore(t)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	if e = s.Bootstrap("admin", "test-password-123"); e != nil {
		t.Fatal(e)
	}
	a, e := New(s, bytes.Repeat([]byte{1}, 32), false, nil)
	if e != nil {
		t.Fatal(e)
	}
	rr := call(t, a, nil, "POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "test-password-123"})
	if rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	return a, rr.Result().Cookies()[0]
}

func TestStreamMeterUsesLargerSubscriptionCharges(t *testing.T) {
	a, admin := testApp(t)
	route, _ := configured(t, a, admin, "http://127.0.0.1:8096")
	var charges []int
	w := &meterWriter{
		ResponseWriter: httptest.NewRecorder(), app: a, route: route,
		usage: &routeRuntime{}, ctx: context.Background(), stream: true,
		accessCharge: func(n int) error { charges = append(charges, n); return nil },
	}
	for i := 0; i < 16; i++ {
		if n, err := w.Write(bytes.Repeat([]byte("x"), 32*1024)); err != nil || n != 32*1024 {
			t.Fatalf("wrote %d: %v", n, err)
		}
		if i == 0 && w.accessCredit != 224*1024 {
			t.Fatalf("unused credit = %d", w.accessCredit)
		}
	}
	if len(charges) != 2 || charges[0] != 256*1024 || charges[1] != 256*1024 {
		t.Fatalf("charges = %v", charges)
	}
}

func TestStreamMeterShrinksChargeNearQuota(t *testing.T) {
	a, admin := testApp(t)
	route, _ := configured(t, a, admin, "http://127.0.0.1:8096")
	var charges []int
	w := &meterWriter{
		ResponseWriter: httptest.NewRecorder(), app: a, route: route,
		usage: &routeRuntime{}, ctx: context.Background(), stream: true,
		accessCharge: func(n int) error {
			charges = append(charges, n)
			if n > 64*1024 {
				return fmt.Errorf("本周期剩余流量不足")
			}
			return nil
		},
	}
	if n, err := w.Write(bytes.Repeat([]byte("x"), 128*1024)); err != nil || n != 128*1024 {
		t.Fatalf("wrote %d: %v", n, err)
	}
	if len(charges) != 6 || charges[0] != 256*1024 || charges[1] != 128*1024 || charges[2] != 64*1024 || charges[3] != 256*1024 || charges[4] != 128*1024 || charges[5] != 64*1024 {
		t.Fatalf("charges = %v", charges)
	}
}

// Opt in to running the existing behavioral suite against an isolated schema
// in a real PostgreSQL database. The SQLite default remains unchanged.
func testStore(t *testing.T) (*Store, error) {
	t.Helper()
	source := os.Getenv("EMBYRELAY_TEST_POSTGRES_URL")
	if source == "" {
		return Open(filepath.Join(t.TempDir(), "test.db"))
	}
	admin, err := database.Open("postgres", source)
	if err != nil {
		return nil, err
	}
	schema := "test_" + ID()
	if _, err = admin.Exec("CREATE SCHEMA " + schema); err != nil {
		admin.Close()
		return nil, err
	}
	t.Cleanup(func() { admin.Exec("DROP SCHEMA " + schema + " CASCADE"); admin.Close() })
	u, err := url.Parse(source)
	if err != nil {
		return nil, err
	}
	params := u.Query()
	params.Set("search_path", schema)
	u.RawQuery = params.Encode()
	return openPostgres(u.String())
}
func call(t *testing.T, a http.Handler, cookie *http.Cookie, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var b []byte
	if body != nil {
		b = Encode(body)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(b))
	r.RemoteAddr = "127.0.0.1:1234"
	if cookie != nil {
		r.AddCookie(cookie)
	}
	r.Header.Set("X-Requested-With", "EmbyRelay")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}
func configured(t *testing.T, a *App, cookie *http.Cookie, upstream string) (Route, string) {
	t.Helper()
	r := call(t, a, cookie, "POST", "/api/v1/upstreams", Upstream{Name: "test", BaseURL: upstream, Enabled: true, Mode: "proxy", AllowedCIDRs: []string{"127.0.0.1/32"}})
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	ups, _ := list[Upstream](a.Store, "upstream")
	r = call(t, a, cookie, "POST", "/api/v1/routes", Route{Alias: "test", UpstreamID: ups[0].ID, Enabled: true, MaxConcurrent: 100})
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	routes, _ := list[Route](a.Store, "route")
	rr := routes[0]
	r = call(t, a, cookie, "POST", "/api/v1/routes/"+rr.ID+"/keys", map[string]int64{"expires_at": 0})
	if r.Code != 201 {
		t.Fatal(r.Body.String())
	}
	var key map[string]string
	json.Unmarshal(r.Body.Bytes(), &key)
	return rr, key["access_key"]
}
func TestManagementPersistenceAndAuth(t *testing.T) {
	a, c := testApp(t)
	if r := call(t, a, nil, "GET", "/api/v1/upstreams", nil); r.Code != 401 {
		t.Fatal("unauthenticated admin access")
	}
	_, secret := configured(t, a, c, "http://127.0.0.1:8096")
	keys, _ := list[Key](a.Store, "key")
	if len(keys) != 1 || keys[0].Hash == secret || keys[0].Hash != Hash(secret) {
		t.Fatal("key stored incorrectly")
	}
	if e := a.Reload(); e != nil {
		t.Fatal(e)
	}
	if len(a.state.Load().routes) != 1 {
		t.Fatal("route not reloaded")
	}
	r := httptest.NewRequest("POST", "/api/v1/upstreams", strings.NewReader(`{}`))
	r.AddCookie(c)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("CSRF header bypass")
	}
	r.Header.Set("X-Requested-With", "EmbyRelay")
	r.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("CSRF origin bypass")
	}
	raw := call(t, a, c, "GET", "/api/v1/routes/"+keys[0].RouteID+"/keys", nil).Body.String()
	if !strings.Contains(raw, secret) || strings.Contains(raw, keys[0].Hash) || strings.Contains(raw, "encrypted_secret") {
		t.Fatal("keys exposed")
	}
}
func TestOperatorCannotWrite(t *testing.T) {
	a, c := testApp(t)
	r := call(t, a, c, "POST", "/api/v1/users", map[string]string{"username": "viewer", "password": "operator-password", "role": "operator"})
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	login := call(t, a, nil, "POST", "/api/v1/auth/login", map[string]string{"username": "viewer", "password": "operator-password"})
	v := login.Result().Cookies()[0]
	if r = call(t, a, v, "GET", "/api/v1/upstreams", nil); r.Code != 200 {
		t.Fatal("read denied")
	}
	if r = call(t, a, v, "POST", "/api/v1/routes", map[string]string{}); r.Code != 403 {
		t.Fatal("operator mutation allowed")
	}
}
func TestQuotaAtomicAndDurable(t *testing.T) {
	a, _ := testApp(t)
	var ok atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if a.Store.Charge("route", 100, 0, 10) == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 10 {
		t.Fatalf("quota overrun %d", ok.Load())
	}
	ts, e := a.Store.Traffic()
	if e != nil || ts[0].Egress != 100 {
		t.Fatal(ts, e)
	}
}
func TestPlaybackAccountingRewriteAndRevocation(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Relay-Key") != "" || r.Header.Get("Referer") != "" {
			t.Error("proxy credential leaked")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"MediaSources":[{"DirectStreamUrl":"/Videos/1/stream?api_key=emby"}]}`)
	}))
	defer up.Close()
	a, c := testApp(t)
	route, key := configured(t, a, c, up.URL)
	gw := httptest.NewServer(a)
	defer gw.Close()
	path := "/r/test/" + key + "/Items/1/PlaybackInfo"
	req, _ := http.NewRequest("GET", gw.URL+path, nil)
	req.Header.Set("Referer", gw.URL+path)
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	body, e := io.ReadAll(resp.Body)
	resp.Body.Close()
	if e != nil || resp.StatusCode != 200 || !strings.Contains(string(body), "/r/test/"+key+"/Videos/1/stream") {
		t.Fatalf("bad rewrite %d %s %v", resp.StatusCode, body, e)
	}
	traffic, _ := a.Store.Traffic()
	if len(traffic) != 1 || traffic[0].Egress != int64(len(body)) || traffic[0].Ingress == 0 {
		t.Fatal("incorrect accounting", traffic)
	}
	keys, _ := list[Key](a.Store, "key")
	call(t, a, c, "DELETE", "/api/v1/access-keys/"+keys[0].ID, nil)
	resp, e = http.Get(gw.URL + path)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("revoked key accepted")
	}
	if r := call(t, a, c, "DELETE", "/api/v1/upstreams/"+route.UpstreamID, nil); r.Code != 400 {
		t.Fatal("deleted referenced upstream")
	}
}
func TestWebSocketUpgradeAndCancellation(t *testing.T) {
	done := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, e := w.(http.Hijacker).Hijack()
		if e != nil {
			return
		}
		defer conn.Close()
		defer close(done)
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		rw.Flush()
		io.Copy(conn, conn)
	}))
	defer up.Close()
	a, c := testApp(t)
	_, key := configured(t, a, c, up.URL)
	gw := httptest.NewServer(a)
	defer gw.Close()
	u, _ := url.Parse(gw.URL)
	conn, e := net.Dial("tcp", u.Host)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "GET /r/test/%s/socket HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n", key, u.Host)
	reader := bufio.NewReader(conn)
	resp, e := http.ReadResponse(reader, nil)
	if e != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade %v %v", resp, e)
	}
	conn.Write([]byte("ping"))
	b := make([]byte, 4)
	if _, e = io.ReadFull(reader, b); e != nil || string(b) != "ping" {
		t.Fatalf("echo %q %v", b, e)
	}
	conn.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream socket leaked")
	}
}
func TestSignedAgentReplayAndReport(t *testing.T) {
	a, c := testApp(t)
	token := call(t, a, c, "POST", "/api/v1/relays/enroll", map[string]any{})
	var obj map[string]any
	json.Unmarshal(token.Body.Bytes(), &obj)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	body := Encode(Enrollment{Token: obj["token"].(string), Name: "node", Endpoint: "http://127.0.0.1:8081", PublicKey: base64.RawStdEncoding.EncodeToString(pub)})
	r := httptest.NewRequest("POST", "/api/v1/agent/enroll", bytes.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1"
	SignRequest(r, body, "", priv)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var enrolled EnrollmentResult
	json.Unmarshal(w.Body.Bytes(), &enrolled)
	r = httptest.NewRequest("GET", "/api/v1/agent/config", nil)
	SignRequest(r, nil, enrolled.ID, priv)
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var signed SignedSnapshot
	json.Unmarshal(w.Body.Bytes(), &signed)
	serverPub, _ := base64.RawStdEncoding.DecodeString(enrolled.ServerKey)
	if _, e := VerifySnapshot(signed, serverPub); e != nil {
		t.Fatal(e)
	}
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 409 {
		t.Fatal("replay accepted")
	}
	signed.Payload = base64.RawStdEncoding.EncodeToString([]byte(`{}`))
	if _, e := VerifySnapshot(signed, serverPub); e == nil {
		t.Fatal("tampered snapshot accepted")
	}
	route, _ := configured(t, a, c, "http://127.0.0.1:8096")
	route.RelayID = enrolled.ID
	a.Store.put("route", route.ID, route, "test")
	a.Reload()
	node, _ := get[Node](a.Store, "node", enrolled.ID)
	hb := Heartbeat{Traffic: []Traffic{{Date: time.Now().UTC().Format("2006-01-02"), RouteID: route.ID, Ingress: 10, Egress: 20}}}
	if e := a.report(node, hb); e != nil {
		t.Fatal(e)
	}
	if e := a.report(node, hb); e != nil {
		t.Fatal(e)
	}
	stats, _ := a.Store.Traffic()
	if len(stats) != 1 || stats[0].Egress != 20 {
		t.Fatal("duplicate report double counted")
	}
}
func TestTOTPAndEncryptedStore(t *testing.T) {
	a, _ := testApp(t)
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	if !totp(secret, "287082", time.Unix(59, 0)) {
		t.Fatal("RFC TOTP vector failed")
	}
	s, e := a.seal(secret)
	if e != nil || strings.Contains(s, secret) {
		t.Fatal("not encrypted")
	}
	v, e := a.unseal(s)
	if e != nil || v != secret {
		t.Fatal(e)
	}
	s = s[:len(s)-2] + "xx"
	if _, e = a.unseal(s); e == nil {
		t.Fatal("tamper accepted")
	}
}

func TestDistributedQuotaReservations(t *testing.T) {
	a, c := testApp(t)
	route, _ := configured(t, a, c, "http://127.0.0.1:8096")
	route.DailyQuota = 100
	route.RelayID = "auto"
	if e := a.Store.put("route", route.ID, route, "test"); e != nil {
		t.Fatal(e)
	}
	a.Reload()
	day := time.Now().UTC().Format("2006-01-02")
	n1, n2 := Node{ID: "one"}, Node{ID: "two"}
	g, e := a.grantQuota(n1, QuotaRequest{RouteID: route.ID, Date: day, Target: 70})
	if e != nil || g.Granted != 70 {
		t.Fatal(g, e)
	}
	g, e = a.grantQuota(n1, QuotaRequest{RouteID: route.ID, Date: day, Target: 70})
	if e != nil || g.Granted != 70 {
		t.Fatal("lease retry not idempotent")
	}
	g, e = a.grantQuota(n2, QuotaRequest{RouteID: route.ID, Date: day, Target: 70})
	if e != nil || g.Granted != 30 {
		t.Fatal("reservation exceeded quota", g, e)
	}
	if e = a.Store.Charge(route.ID, 100, 0, 1); e != ErrQuota {
		t.Fatal("local fallback reused agent budget", e)
	}
	if e = a.report(n1, Heartbeat{Traffic: []Traffic{{Date: day, RouteID: route.ID, Egress: 20}}}); e != nil {
		t.Fatal(e)
	}
	if e = a.Store.Charge(route.ID, 100, 0, 1); e != ErrQuota {
		t.Fatal("reported usage freed spent quota", e)
	}
}

func TestStoreReopenAndRateLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "durable.db")
	store, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = store.put("upstream", "persist", Upstream{ID: "persist", Name: "persist", BaseURL: "https://example.com", Mode: "proxy"}, "test"); e != nil {
		t.Fatal(e)
	}
	store.Close()
	store, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	saved, e := get[Upstream](store, "upstream", "persist")
	if e != nil || saved.Name != "persist" {
		t.Fatal("persistence failed")
	}
	w := &meterWriter{route: Route{RateBPS: 1000}, usage: &routeRuntime{}, ctx: context.Background()}
	start := time.Now()
	if e = w.wait(100); e != nil {
		t.Fatal(e)
	}
	if time.Since(start) < 90*time.Millisecond {
		t.Fatal("rate limiter did not wait")
	}
	l := loginLimiter{}
	for i := 0; i < 10; i++ {
		if !l.allow("127.0.0.1:1") {
			t.Fatal("early throttle")
		}
	}
	if l.allow("127.0.0.1:2") {
		t.Fatal("login limit bypassed")
	}
}

func TestExpiredKeyAndCookieIsolation(t *testing.T) {
	a, c := testApp(t)
	var gotCookies string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookies = r.Header.Get("Cookie")
		w.Write([]byte("ok"))
	}))
	defer up.Close()
	_, key := configured(t, a, c, up.URL)
	req := httptest.NewRequest("GET", "/r/test/"+key+"/Items", nil)
	req.AddCookie(c)
	req.AddCookie(&http.Cookie{Name: "emby", Value: "valid"})
	resp := httptest.NewRecorder()
	a.ServeHTTP(resp, req)
	if strings.Contains(gotCookies, "relay_session") || !strings.Contains(gotCookies, "emby=valid") {
		t.Fatal("cookie isolation failed")
	}
	keys, _ := list[Key](a.Store, "key")
	keys[0].ExpiresAt = time.Now().Add(-time.Second).Unix()
	a.Store.put("key", keys[0].ID, keys[0], "test")
	a.Reload()
	resp = call(t, a, nil, "GET", "/r/test/"+key+"/Items", nil)
	if resp.Code != 401 {
		t.Fatal("expired key accepted")
	}
}
