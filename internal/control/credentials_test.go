package control

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEncryptedEmbyCredentials(t *testing.T) {
	a, cookie := testApp(t)
	w := call(t, a, cookie, "POST", "/api/v1/upstreams", map[string]any{"name": "demo", "base_url": "example.com", "mode": "redirect", "emby_username": "emby-person", "emby_password": "private-password"})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	us, err := list[Upstream](a.Store, "upstream")
	if err != nil || len(us) != 1 {
		t.Fatal(err, us)
	}
	u := us[0]
	if u.EncryptedCredentials == "" || strings.Contains(string(Encode(u)), "private-password") || strings.Contains(string(Encode(u)), "emby-person") {
		t.Fatal("credentials not encrypted")
	}
	if u.Mode != "proxy" {
		t.Fatal("legacy mode not normalized")
	}
	for _, path := range []string{"/api/v1/upstreams", "/api/v1/audit"} {
		out := call(t, a, cookie, "GET", path, nil).Body.String()
		if strings.Contains(out, "private-password") || strings.Contains(out, u.EncryptedCredentials) {
			t.Fatal("secret leaked", path)
		}
	}
	snap, err := a.Store.Snapshot()
	if err != nil || strings.Contains(string(Encode(snap)), u.EncryptedCredentials) || !snap.AutoMediaRedirect {
		t.Fatal("snapshot leaked credentials or retained restrictions")
	}
	path := "/api/v1/upstreams/" + u.ID
	reveal := func() embyCredentials {
		t.Helper()
		w := call(t, a, cookie, "POST", path+"/credentials", map[string]any{})
		var c embyCredentials
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &c) != nil || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(w.Code, w.Body.String())
		}
		return c
	}
	if c := reveal(); c.Username != "emby-person" || c.Password != "private-password" {
		t.Fatal("reveal mismatch")
	}
	w = call(t, a, cookie, "PATCH", path, map[string]any{"name": "renamed", "encrypted_credentials": "tampered"})
	if w.Code != 200 || reveal().Password != "private-password" {
		t.Fatal("edit lost password")
	}
	w = call(t, a, cookie, "PATCH", path, map[string]any{"emby_password": "replacement"})
	if w.Code != 200 || reveal().Password != "replacement" {
		t.Fatal("password update failed")
	}
	call(t, a, cookie, "POST", "/api/v1/users", map[string]string{"username": "reader", "password": "reader-password", "role": "operator"})
	login := call(t, a, nil, "POST", "/api/v1/auth/login", map[string]string{"username": "reader", "password": "reader-password"})
	if login.Code != 200 {
		t.Fatal(login.Body.String())
	}
	if w := call(t, a, login.Result().Cookies()[0], "POST", path+"/credentials", map[string]any{}); w.Code != 403 {
		t.Fatal("operator exposed password", w.Code)
	}
	if w := call(t, a, nil, "POST", path+"/credentials", map[string]any{}); w.Code != 401 {
		t.Fatal("anonymous exposed password")
	}
	w = call(t, a, cookie, "PATCH", path, map[string]any{"clear_credentials": true})
	if w.Code != 200 || reveal().Password != "" || reveal().Username != "" {
		t.Fatal("clear failed")
	}
}
