package control

import "testing"

func TestNormalizeUpstreamURL(t *testing.T) {
	for input, want := range map[string]string{
		" emby.example.com ":       "https://emby.example.com:443",
		"emby.example.com:8920":    "https://emby.example.com:8920",
		"http://emby.example.com":  "http://emby.example.com:80",
		"http://192.168.1.20:8096": "http://192.168.1.20:8096",
		"//emby.example.com/emby":  "https://emby.example.com:443/emby",
		"[2001:db8::1]:8920":       "https://[2001:db8::1]:8920",
	} {
		got, err := normalizeUpstreamURL(input)
		if err != nil || got != want {
			t.Errorf("%q: %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "https://host:", "host:65536", "ftp://host", "https://user:pass@host", "host?api_key=secret"} {
		if _, err := normalizeUpstreamURL(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}
