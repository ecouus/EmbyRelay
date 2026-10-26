package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestKeyLookupByDigest(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer up.Close()
	a, c := testApp(t)
	route, secret := configured(t, a, c, up.URL)
	status := func(path string) int { return call(t, a, nil, "GET", "/r/test/"+path+"/System/Info", nil).Code }
	if status(secret) != 200 || status("wrong-key-value") != 401 {
		t.Fatal("baseline lookup failed")
	}
	keys, _ := list[Key](a.Store, "key")
	good := keys[0]
	put := func(k Key) {
		t.Helper()
		if err := a.Store.put("key", k.ID, k, "test"); err != nil {
			t.Fatal(err)
		}
	}
	reload := func() {
		t.Helper()
		if err := a.Reload(); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 20; i++ {
		put(Key{ID: ID(), RouteID: route.ID, Hash: Hash(ID()), Enabled: true})
	}
	put(Key{ID: ID(), RouteID: route.ID, Hash: good.Hash, Enabled: false})
	put(Key{ID: ID(), RouteID: route.ID, Hash: good.Hash, Enabled: true, ExpiresAt: time.Now().Add(-time.Hour).Unix()})
	reload()
	if status(secret) != 200 {
		t.Fatal("valid key lost among duplicates")
	}
	good.Enabled = false
	put(good)
	reload()
	if status(secret) != 401 {
		t.Fatal("disabled or expired duplicates accepted")
	}
	good.Enabled = true
	put(good)
	route.OwnerAccountID = "owner"
	if err := a.Store.put("route", route.ID, route, "test"); err != nil {
		t.Fatal(err)
	}
	reload()
	if status(secret) != 401 {
		t.Fatal("account mismatch accepted")
	}
	good.AccountID = "owner"
	put(good)
	reload()
	if w := call(t, a, nil, "GET", "/r/test/"+secret+"/System/Info", nil); strings.Contains(w.Body.String(), "invalid or expired access key") {
		t.Fatal("matching account rejected")
	}
}

func TestClientIPTrustedProxiesTrackEnvironment(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.2:1234"
	r.Header.Set("X-Forwarded-For", "5.6.7.8")
	for _, tc := range []struct{ env, want string }{{"10.0.0.2/32", "5.6.7.8"}, {"10.0.0.2/32", "5.6.7.8"}, {"", "10.0.0.2"}, {"bad, 10.0.0.0/24", "5.6.7.8"}, {"192.168.0.0/16", "10.0.0.2"}} {
		t.Setenv("RELAY_TRUSTED_PROXIES", tc.env)
		if got := clientIP(r); got != tc.want {
			t.Fatalf("env %q: got %s want %s", tc.env, got, tc.want)
		}
	}
}

func TestPatchUserKeepsPasswordWhenBlank(t *testing.T) {
	a, admin := testApp(t)
	if r := call(t, a, admin, "POST", "/api/v1/users", map[string]string{"username": "ops", "role": "operator"}); r.Code != 400 {
		t.Fatal("POST without password accepted", r.Code)
	}
	if r := call(t, a, admin, "POST", "/api/v1/users", map[string]string{"username": "ops", "role": "operator", "password": "ops-password-1"}); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	var id string
	if err := a.Store.DB.QueryRow("SELECT id FROM users WHERE username='ops'").Scan(&id); err != nil {
		t.Fatal(err)
	}
	login := func(pass string) *httptest.ResponseRecorder {
		return call(t, a, nil, "POST", "/api/v1/auth/login", map[string]string{"username": "ops", "password": pass})
	}
	session := login("ops-password-1")
	if session.Code != 200 {
		t.Fatal(session.Body.String())
	}
	cookie := session.Result().Cookies()[0]
	if call(t, a, cookie, "GET", "/api/v1/auth/me", nil).Code != 200 {
		t.Fatal("session not valid")
	}
	patch := func(body map[string]string) int {
		return call(t, a, admin, "PATCH", "/api/v1/users/"+id, body).Code
	}
	if patch(map[string]string{"username": "ops", "role": "operator"}) != 200 {
		t.Fatal("patch without password rejected")
	}
	if call(t, a, cookie, "GET", "/api/v1/auth/me", nil).Code != 200 {
		t.Fatal("unchanged patch revoked session")
	}
	if patch(map[string]string{"username": "ops", "role": "admin"}) != 200 {
		t.Fatal("role patch rejected")
	}
	if call(t, a, cookie, "GET", "/api/v1/auth/me", nil).Code == 200 {
		t.Fatal("role change kept old session")
	}
	if login("ops-password-1").Code != 200 {
		t.Fatal("old password lost")
	}
	if patch(map[string]string{"username": "ops", "role": "admin", "password": "new-password-22"}) != 200 {
		t.Fatal("password patch rejected")
	}
	if login("ops-password-1").Code == 200 || login("new-password-22").Code != 200 {
		t.Fatal("password change not applied")
	}
	if call(t, a, admin, "PATCH", "/api/v1/users/missing", map[string]string{"username": "xyz", "role": "admin"}).Code != 400 {
		t.Fatal("missing user accepted")
	}
}
