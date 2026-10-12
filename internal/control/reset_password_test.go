package control

import "testing"

func TestResetAdminPassword(t *testing.T) {
	a, cookie := testApp(t)
	if err := a.Store.ResetAdminPassword("missing", "new-pass-123"); err == nil {
		t.Fatal("missing user accepted")
	}
	if err := a.Store.ResetAdminPassword("admin", "short"); err == nil {
		t.Fatal("short password accepted")
	}
	if err := a.Store.ResetAdminPassword("admin", "new-pass-123"); err != nil {
		t.Fatal(err)
	}
	if r := call(t, a, cookie, "GET", "/api/v1/auth/me", nil); r.Code != 401 {
		t.Fatal("old session still valid")
	}
	if r := call(t, a, nil, "POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "test-password-123"}); r.Code != 401 {
		t.Fatal("old password accepted")
	}
	if r := call(t, a, nil, "POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "new-pass-123"}); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
}
