package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestVerifyEmbySource(t *testing.T) {
	for _, tc := range []struct{ name, failure string }{
		{"valid", ""}, {"public path unavailable", "public404"}, {"public path forbidden", "public403"}, {"public endpoint failed", "public500"}, {"user endpoint failed", "user500"}, {"both probes failed", "both500"}, {"public failed and bad password", "public500password"}, {"HTML", "html"}, {"fake info", "info"}, {"bad password", "password"}, {"missing token", "token"}, {"wrong server", "server"}, {"invalid token", "me"}, {"different user", "user"}, {"administrator", "admin"}, {"redirect", "redirect"}, {"oversized", "large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var loggedOut atomic.Bool
			var loginRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/emby/System/Info/Public":
					switch tc.failure {
					case "public404":
						w.WriteHeader(404)
						return
					case "public403":
						w.WriteHeader(403)
						return
					case "public500", "public500password", "both500":
						w.WriteHeader(500)
						return
					case "html":
						fmt.Fprint(w, "<html>Emby</html>")
						return
					case "large":
						fmt.Fprint(w, strings.Repeat(" ", 1<<20)+`{}`)
						return
					case "info":
						writeJSON(w, 200, map[string]string{"ServerName": "fake"})
						return
					case "redirect":
						http.Redirect(w, r, "/trap", 302)
						return
					}
					writeJSON(w, 200, map[string]string{"Id": "server", "ServerName": "Emby", "Version": "4.9.0.0"})
				case "/emby/Users/AuthenticateByName":
					loginRequests.Add(1)
					var credentials map[string]string
					json.NewDecoder(r.Body).Decode(&credentials)
					if credentials["Username"] != "viewer" || credentials["Pw"] != "secret-password" || r.Header.Get("X-Emby-Authorization") == "" {
						t.Error("incorrect login request")
					}
					if tc.failure == "password" || tc.failure == "public500password" {
						w.WriteHeader(401)
						return
					}
					token, serverID := "secret-token", "server"
					if tc.failure == "token" {
						token = ""
					}
					if tc.failure == "server" {
						serverID = "other"
					}
					writeJSON(w, 200, map[string]any{"AccessToken": token, "ServerId": serverID, "User": map[string]any{"Id": "viewer", "Policy": map[string]bool{"IsAdministrator": tc.failure == "admin"}}})
				case "/emby/Users/Me":
					if tc.failure == "user500" || tc.failure == "both500" {
						w.WriteHeader(500)
						return
					}
					if r.Header.Get("X-Emby-Token") != "secret-token" {
						t.Error("token not sent")
					}
					if tc.failure == "me" {
						w.WriteHeader(401)
						return
					}
					id := "viewer"
					if tc.failure == "user" {
						id = "other"
					}
					writeJSON(w, 200, map[string]string{"Id": id, "ServerId": "server"})
				case "/emby/Sessions/Logout":
					loggedOut.Store(true)
					w.WriteHeader(204)
				default:
					t.Error("unexpected path", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			u := Upstream{BaseURL: server.URL + "/emby", AllowedCIDRs: []string{"127.0.0.1/32"}}
			err := verifyEmbySource(context.Background(), u, embyCredentials{Username: "viewer", Password: "secret-password"})
			if (err != nil) != (tc.failure != "" && tc.failure != "public404" && tc.failure != "public403" && tc.failure != "public500" && tc.failure != "user500") {
				t.Fatalf("unexpected result: %v", err)
			}
			if err != nil && (strings.Contains(err.Error(), "secret-password") || strings.Contains(err.Error(), "secret-token")) {
				t.Fatal("credentials exposed")
			}
			if (tc.failure == "" || tc.failure == "me" || tc.failure == "user" || tc.failure == "user500" || tc.failure == "both500") && !loggedOut.Load() {
				t.Error("validation session not revoked")
			}
			if (tc.failure == "html" || tc.failure == "info" || tc.failure == "redirect" || tc.failure == "large") && loginRequests.Load() != 0 {
				t.Error("credentials sent before server validation")
			}
			u.AllowedCIDRs = nil
			if err := verifyEmbySource(context.Background(), u, embyCredentials{Username: "viewer", Password: "secret-password"}); err == nil {
				t.Error("private source accepted")
			}
		})
	}
}

func TestPortalValidationFailurePreservesSource(t *testing.T) {
	a, _, _, _ := subscriptionFixture(t)
	cookie := portalLogin(t, a, "viewer")
	calls := 0
	a.verifySource = func(_ context.Context, u Upstream, c embyCredentials) error {
		calls++
		if u.BaseURL != "https://media.example.com:443" || c.Username != "real-user" || c.Password != "real-password" {
			t.Error("wrong candidate verified")
		}
		if calls > 1 {
			return fmt.Errorf("账号验证失败")
		}
		return nil
	}
	body := map[string]string{"name": "valid", "base_url": "media.example.com", "emby_username": "real-user", "emby_password": "real-password"}
	if w := call(t, a, cookie, "POST", "/api/v1/portal/sources", body); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	sources, _ := list[Upstream](a.Store, "upstream")
	var source Upstream
	for _, u := range sources {
		if u.OwnerAccountID != "" {
			source = u
		}
	}
	if source.ID == "" {
		t.Fatal("source missing")
	}
	before := string(Encode(source))
	body["name"] = "invalid"
	delete(body, "emby_username")
	delete(body, "emby_password")
	if w := call(t, a, cookie, "PATCH", "/api/v1/portal/sources/"+source.ID, body); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	after, _ := get[Upstream](a.Store, "upstream", source.ID)
	if string(Encode(after)) != before {
		t.Fatal("failed verification changed source")
	}
	if w := call(t, a, cookie, "POST", "/api/v1/portal/sources", map[string]string{"name": "invalid", "base_url": "media.example.com", "emby_username": "real-user", "emby_password": "real-password"}); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	sources, _ = list[Upstream](a.Store, "upstream")
	if len(sources) != 2 {
		t.Fatal("failed verification persisted source")
	}
}
