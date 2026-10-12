package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestProgressForwardingAndDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, header, value, query string
		status                     int
	}{
		{"token", "X-Emby-Token", "secret-token", "", 204},
		{"emby auth", "X-Emby-Authorization", `MediaBrowser Client="Hills", Token="secret-token"`, "", 401},
		{"authorization", "Authorization", "Bearer secret-token", "", 401},
		{"media browser", "X-MediaBrowser-Authorization", `MediaBrowser Token="secret-token"`, "", 401},
		{"query", "", "", "api_key=secret-token", 401},
		{"absent", "", "", "", 401},
		{"hop header", "X-Emby-Token", "secret-token", "", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"ItemId":"item","PositionTicks":123456}`
			calls := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				data, _ := io.ReadAll(r.Body)
				if r.Method != "POST" || r.URL.Path != "/emby/Sessions/Playing/Progress" || string(data) != body || r.Header.Get("Content-Type") != "application/json" {
					t.Error("method, path or body changed")
				}
				if tc.header != "" && tc.name != "hop header" && r.Header.Get(tc.header) != tc.value {
					t.Error("authentication header lost")
				}
				if r.URL.RawQuery != tc.query {
					t.Error("query changed")
				}
				w.WriteHeader(tc.status)
			}))
			defer up.Close()
			u, _ := ValidateURL(up.URL)
			g := New(Config{Upstream: u, TrustedAuth: true, Network: NetworkPolicy{Allowed: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}})
			defer g.CloseIdleConnections()
			r := httptest.NewRequest("POST", "http://relay/emby/Sessions/Playing/Progress?"+tc.query, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			if tc.header != "" {
				r.Header.Set(tc.header, tc.value)
			}
			if tc.name == "hop header" {
				r.Header.Set("Connection", "X-Emby-Token")
			}
			var d Detail
			var reason string
			r = r.WithContext(WithDiagnostic(WithDetail(r.Context(), func(v Detail) { d = v }), func(v string) { reason = v }))
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)
			if w.Code != tc.status || calls != 1 {
				t.Fatalf("status=%d calls=%d", w.Code, calls)
			}
			if tc.status == 401 {
				expected := "upstream_auth_rejected"
				if tc.name == "absent" {
					expected = "inbound_auth_missing"
				}
				if tc.name == "hop header" {
					expected = "outbound_auth_missing"
				}
				if d.Code != expected || !strings.Contains(reason, "播放进度上报") {
					t.Fatalf("%+v %s", d, reason)
				}
			}
			if d.Auth == nil || d.Auth.Operation != "playback_progress" || (tc.name != "hop header" && strings.Join(d.Auth.Inbound, ",") != strings.Join(d.Auth.Outbound, ",")) {
				t.Fatalf("bad trace: %+v", d.Auth)
			}
			encoded, _ := json.Marshal(d)
			if strings.Contains(string(encoded), "secret-token") || strings.Contains(string(encoded), "Hills") {
				t.Fatal("credential value leaked")
			}
		})
	}
}
