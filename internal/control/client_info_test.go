package control

import (
	"net/http/httptest"
	"testing"
)

func TestClientMetadata(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.2:1234"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")
	t.Setenv("RELAY_TRUSTED_PROXIES", "")
	if clientIP(r) != "10.0.0.2" {
		t.Fatal("untrusted forwarded header accepted")
	}
	t.Setenv("RELAY_TRUSTED_PROXIES", "10.0.0.2/32")
	if clientIP(r) != "5.6.7.8" {
		t.Fatal("incorrect trust traversal")
	}
	r.Header.Set("X-Emby-Authorization", `MediaBrowser Client="Hills", Version="1.2", Token="secret", DeviceId="private"`)
	name, version := clientApp(r)
	if name != "Hills" || version != "1.2" {
		t.Fatal(name, version)
	}
}

func TestLoopbackProxyClientIP(t *testing.T) {
	t.Setenv("RELAY_TRUSTED_PROXIES", "127.0.0.1/32")
	for _, tc := range []struct{ name, peer, forwarded, want string }{
		{"local proxy", "127.0.0.1:1234", "203.0.113.7", "203.0.113.7"},
		{"IPv6 client", "127.0.0.1:1234", "2001:db8::7", "2001:db8::7"},
		{"spoofed prefix", "127.0.0.1:1234", "198.51.100.1, 203.0.113.7", "203.0.113.7"},
		{"untrusted bridge", "172.18.0.1:1234", "203.0.113.7", "172.18.0.1"},
		{"direct spoof", "203.0.113.9:1234", "198.51.100.1", "203.0.113.9"},
		{"missing header", "127.0.0.1:1234", "", "127.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.peer
			r.Header.Set("X-Forwarded-For", tc.forwarded)
			if got := clientIP(r); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
