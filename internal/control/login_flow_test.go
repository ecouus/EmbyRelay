package control

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestTwoStepLogin(t *testing.T) {
	a, _ := testApp(t)
	encrypted, err := a.seal("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.Store.DB.Exec("UPDATE users SET totp=? WHERE username='admin'", encrypted); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		pass, code string
		status     int
		required   bool
	}{
		{"wrong-password", "", 401, false},
		{"test-password-123", "", 200, true},
		{"test-password-123", "invalid", 401, false},
	} {
		r := call(t, a, nil, "POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": tc.pass, "code": tc.code})
		if r.Code != tc.status || strings.Contains(r.Body.String(), `"totp_required":true`) != tc.required {
			t.Fatalf("unexpected response: %d %s", r.Code, r.Body.String())
		}
		if len(r.Result().Cookies()) != 0 {
			t.Fatal("session issued before TOTP verification")
		}
	}
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(time.Now().Unix()/30))
	h := hmac.New(sha1.New, []byte("12345678901234567890"))
	h.Write(b)
	sum := h.Sum(nil)
	p := sum[len(sum)-1] & 15
	code := fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[p:p+4])&0x7fffffff)%1000000)
	r := call(t, a, nil, "POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "test-password-123", "code": code})
	if r.Code != 200 || len(r.Result().Cookies()) != 1 {
		t.Fatalf("login failed: %d %s", r.Code, r.Body.String())
	}
}
