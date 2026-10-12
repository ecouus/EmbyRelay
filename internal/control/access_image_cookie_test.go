package control

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSubscriptionImageCookieAuthorization(t *testing.T) {
	a, _, account, prefix := subscriptionFixture(t)
	a.SecureCookies = true
	login := call(t, a, nil, "POST", prefix+"/emby/Users/AuthenticateByName", map[string]string{"Username": "viewer", "Pw": "viewer-password"})
	if login.Code != 200 {
		t.Fatal(login.Code, login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != subscriptionCookie || cookies[0].Path != prefix+"/" || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].Value == "" || cookies[0].Value == "source-secret-token" {
		t.Fatal("missing or unsafe subscription cookie")
	}
	cookie := cookies[0]
	request := func(method, path, explicit string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, prefix+path, nil)
		r.AddCookie(cookie)
		if explicit != "" {
			r.Header.Set("X-Emby-Token", explicit)
		}
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "/emby/Items/1/Images/Primary", ""); w.Code != 200 || w.Body.String() != "test-image" {
		t.Fatal("cookie image failed", w.Code, w.Body.String())
	}
	for _, path := range []string{"/Videos/1/stream", "/Items/1/PlaybackInfo", "/emby/Items/1/Images/Primary/../../Videos/1/stream"} {
		if w := request("GET", path, ""); w.Code != 401 {
			t.Fatal("cookie authorized non-image request", path, w.Code)
		}
	}
	if w := request("POST", "/emby/Items/1/Images/Primary", ""); w.Code != 401 {
		t.Fatal("cookie authorized image mutation", w.Code)
	}
	if w := request("GET", "/emby/Items/1/Images/Primary", "invalid-explicit-token"); w.Code == 200 {
		t.Fatal("cookie rescued invalid explicit token")
	}
	account.Enabled = false
	if err := a.Store.put("access_account", account.ID, account, "test"); err != nil {
		t.Fatal(err)
	}
	if w := request("GET", "/emby/Items/1/Images/Primary", ""); w.Code == 200 {
		t.Fatal("disabled account accepted cookie")
	}
	account.Enabled = true
	if err := a.Store.put("access_account", account.ID, account, "test"); err != nil {
		t.Fatal(err)
	}
	keys, err := list[Key](a.Store, "key")
	if err != nil || len(keys) != 1 {
		t.Fatal("fixture key unavailable")
	}
	other := keys[0]
	other.ID, other.Hash = ID(), Hash("different-entry-key")
	if err := a.Store.put("key", other.ID, other, "test"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := a.Store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(snapshot); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/r/sub/different-entry-key/emby/Items/1/Images/Primary", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code == 200 {
		t.Fatal("cookie crossed access-key boundary")
	}
	logout := request("POST", "/Sessions/Logout", cookie.Value)
	if logout.Code != 204 {
		t.Fatal("logout failed", logout.Code)
	}
	cleared := logout.Result().Cookies()
	if len(cleared) != 1 || cleared[0].Name != subscriptionCookie || cleared[0].MaxAge >= 0 || cleared[0].Path != prefix+"/" {
		t.Fatal("logout did not clear scoped cookie")
	}
	if w := request("GET", "/emby/Items/1/Images/Primary", ""); w.Code == 200 {
		t.Fatal("revoked cookie still authorized images")
	}
}

func TestImageCookieRejectsConflictingTokens(t *testing.T) {
	r := httptest.NewRequest("GET", "/emby/Items/1/Images/Primary?api_key=one", nil)
	r.Header.Set("X-Emby-Token", "two")
	r.AddCookie(&http.Cookie{Name: subscriptionCookie, Value: "valid-cookie"})
	if requestAccessToken(r, true) != "" {
		t.Fatal("conflicting explicit tokens rescued")
	}
	r = httptest.NewRequest("GET", "/emby/Items/1/Images/Primary", nil)
	r.AddCookie(&http.Cookie{Name: subscriptionCookie, Value: "one"})
	r.AddCookie(&http.Cookie{Name: subscriptionCookie, Value: "two"})
	if requestAccessToken(r, true) != "" {
		t.Fatal("conflicting cookies accepted")
	}
}
