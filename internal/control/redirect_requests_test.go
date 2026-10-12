package control

import "testing"

func TestRetiredRedirectApprovalPermissions(t *testing.T) {
	a, admin := testApp(t)
	call(t, a, admin, "POST", "/api/v1/users", map[string]string{"username": "viewer", "password": "operator-password", "role": "operator"})
	login := call(t, a, nil, "POST", "/api/v1/auth/login", map[string]string{"username": "viewer", "password": "operator-password"})
	if login.Code != 200 {
		t.Fatal(login.Code)
	}
	c := login.Result().Cookies()[0]
	if r := call(t, a, c, "POST", "/api/v1/redirect-requests", map[string]string{"diagnostic_id": "old"}); r.Code != 410 {
		t.Fatal(r.Code)
	}
	if r := call(t, a, admin, "PATCH", "/api/v1/redirect-requests/old", map[string]string{"action": "approve"}); r.Code != 410 {
		t.Fatal(r.Code)
	}
}
