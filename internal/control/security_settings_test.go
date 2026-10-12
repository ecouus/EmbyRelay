package control

import "testing"

func TestRetiredSecurityMode(t *testing.T) {
	a, cookie := testApp(t)
	a.Store.DB.Exec("INSERT INTO settings(key,value) VALUES('auto_media_redirect','false')")
	s, err := a.Store.Snapshot()
	if err != nil || !s.AutoMediaRedirect {
		t.Fatal("old setting prevented automatic relay")
	}
	if w := call(t, a, cookie, "PATCH", "/api/v1/settings/security", map[string]bool{"redirect_safe_mode": true}); w.Code != 410 {
		t.Fatal(w.Code)
	}
	if w := call(t, a, cookie, "POST", "/api/v1/redirect-requests", map[string]string{}); w.Code != 410 {
		t.Fatal(w.Code)
	}
}
