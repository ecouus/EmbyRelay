package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
)

func TestAutomaticMediaRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-MediaBrowser-Authorization") != "" {
			t.Error("cross-origin credentials leaked")
		}
		w.Write([]byte("video"))
	}))
	defer target.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL+"/stream", 302) }))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	for _, auto := range []bool{false, true} {
		for _, mode := range []string{"proxy", "redirect", "auto"} {
			g := New(Config{Upstream: u, TrustedAuth: true, Mode: mode, AutoMediaRedirect: auto, Network: NetworkPolicy{Allowed: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}})
			r := httptest.NewRequest("GET", "http://relay/Videos/id/stream", nil)
			r.Header.Set("Authorization", "secret")
			r.Header.Set("X-MediaBrowser-Authorization", "secret")
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)
			if auto {
				if w.Code != 200 || w.Body.String() != "video" {
					t.Fatal(mode, w.Code, w.Body.String())
				}
			} else if w.Code != 502 {
				t.Fatal("default must reject unknown host", w.Code)
			}
			// Cross-origin API redirects remain rejected when media redirects are enabled.
			r = httptest.NewRequest("GET", "http://relay/emby/Users", nil)
			w = httptest.NewRecorder()
			g.ServeHTTP(w, r)
			if w.Code != 502 {
				t.Fatal("API external redirect allowed")
			}
			g.CloseIdleConnections()
		}
	}
	// Media redirects require explicit permission to reach loopback addresses.
	tr := &policyTransport{cfg: Config{Upstream: u, AutoMediaRedirect: true}}
	targetURL, _ := url.Parse(target.URL)
	if err := tr.cfg.Network.CheckURL(t.Context(), targetURL); err == nil {
		t.Fatal("private address allowed")
	}
	https, _ := url.Parse("https://source.example")
	tr.cfg.Upstream = https
	for _, raw := range []string{"http://cdn.example/file", "https://user:pass@cdn.example/file", "file:///tmp/file", "https://cdn.example/file#fragment"} {
		target, _ := url.Parse(raw)
		if tr.allowed(target, false) {
			t.Fatal("unsafe redirect accepted", raw)
		}
	}
}

func TestHTTPSMediaDowngrade(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Emby-Token") != "" {
			t.Error("token leaked")
		}
		w.Write([]byte("video"))
	}))
	defer target.Close()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/file", http.StatusFound)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	for _, auto := range []bool{false, true} {
		for _, path := range []string{"/emby/videos/639474/original.mkv", "/Videos/id/stream", "/emby/Users"} {
			tr := &policyTransport{base: upstream.Client().Transport.(*http.Transport), cfg: Config{Upstream: u, AutoMediaRedirect: auto, Network: NetworkPolicy{Allowed: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}}}
			r, _ := http.NewRequest("GET", upstream.URL+path, nil)
			r.Header.Set("X-Emby-Token", "secret")
			resp, err := tr.RoundTrip(r)
			if auto && path != "/emby/Users" {
				if err != nil {
					t.Fatal(path, err)
				}
				if resp.StatusCode != 200 {
					t.Fatal(resp.StatusCode)
				}
				resp.Body.Close()
			} else if err == nil {
				resp.Body.Close()
				t.Fatal("restricted redirect accepted", auto, path)
			}
		}
	}
}

func TestDefaultPortOrigin(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"https://emby.example:443", "https://emby.example", true},
		{"http://emby.example:80", "http://emby.example", true},
		{"https://emby.example:8920", "https://emby.example", false},
		{"https://emby.example", "http://emby.example", false},
	} {
		a, _ := url.Parse(tc.a)
		b, _ := url.Parse(tc.b)
		if sameOrigin(a, b) != tc.same {
			t.Fatal(tc)
		}
	}
}
