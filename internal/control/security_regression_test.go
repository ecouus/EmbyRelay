package control

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestOperatorCanManageOwnSecurityWithoutAdminRights(t *testing.T) {
	a, admin := testApp(t)
	r := call(t, a, admin, "POST", "/api/v1/users", map[string]string{"username": "operator", "password": "operator-password", "role": "operator"})
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	login := call(t, a, nil, "POST", "/api/v1/auth/login", map[string]string{"username": "operator", "password": "operator-password"})
	cookie := login.Result().Cookies()[0]
	r = call(t, a, cookie, "POST", "/api/v1/auth/password", map[string]string{"current": "operator-password", "next": "operator-new-password"})
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	if r = call(t, a, cookie, "GET", "/api/v1/auth/me", nil); r.Code != 401 {
		t.Fatal("old session survived password change")
	}
	login = call(t, a, nil, "POST", "/api/v1/auth/login", map[string]string{"username": "operator", "password": "operator-new-password"})
	cookie = login.Result().Cookies()[0]
	setup := call(t, a, cookie, "POST", "/api/v1/auth/totp", map[string]string{"action": "setup", "password": "operator-new-password"})
	if setup.Code != 200 {
		t.Fatal(setup.Code, setup.Body.String())
	}
	var data map[string]string
	if err := json.Unmarshal(setup.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(data["secret"])
	if err != nil {
		t.Fatal(err)
	}
	counter := make([]byte, 8)
	binary.BigEndian.PutUint64(counter, uint64(time.Now().Unix()/30))
	hash := hmac.New(sha1.New, key)
	hash.Write(counter)
	sum := hash.Sum(nil)
	offset := sum[len(sum)-1] & 15
	code := fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[offset:offset+4])&0x7fffffff)%1000000)
	r = call(t, a, cookie, "POST", "/api/v1/auth/totp", map[string]string{"action": "enable", "password": "operator-new-password", "secret": data["secret"], "code": code})
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	login = call(t, a, nil, "POST", "/api/v1/auth/login", map[string]string{"username": "operator", "password": "operator-new-password", "code": code})
	if login.Code != http.StatusOK || len(login.Result().Cookies()) == 0 {
		t.Fatal(login.Body.String())
	}
	cookie = login.Result().Cookies()[0]
	if r = call(t, a, cookie, "POST", "/api/v1/routes", map[string]string{}); r.Code != 403 {
		t.Fatal("operator gained admin access")
	}
	if r = call(t, a, cookie, "DELETE", "/api/v1/auth/password", nil); r.Code != 403 {
		t.Fatal("security exception allowed other methods")
	}
	if r = call(t, a, admin, "GET", "/api/v1/auth/me", nil); r.Code != 200 {
		t.Fatal("operator changed administrator session")
	}
}

func TestFailedUsernameCannotBlockDifferentIP(t *testing.T) {
	var limiter loginLimiter
	for i := 0; i < 10; i++ {
		limiter.fail("admin", "198.51.100.1", "ADMIN")
	}
	if limiter.allow("admin", "198.51.100.1", "admin") {
		t.Fatal("attacker IP not limited")
	}
	if !limiter.allow("admin", "198.51.100.2", "admin") {
		t.Fatal("attacker locked out another IP")
	}
	if !limiter.allow("portal", "198.51.100.1", "admin") {
		t.Fatal("limiter scopes interfere")
	}
	if limiter.allow("admin", "198.51.100.1", "another-user") {
		t.Fatal("rotating usernames bypassed IP limit")
	}
}
