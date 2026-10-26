package control

import (
	"net"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"strings"
	"sync"
)

var trustedProxies struct {
	sync.Mutex
	raw      string
	prefixes []netip.Prefix
}

func trustedPrefixes() []netip.Prefix {
	raw := os.Getenv("RELAY_TRUSTED_PROXIES")
	trustedProxies.Lock()
	defer trustedProxies.Unlock()
	if raw != trustedProxies.raw {
		var out []netip.Prefix
		for _, item := range strings.Split(raw, ",") {
			if p, e := netip.ParsePrefix(strings.TrimSpace(item)); e == nil {
				out = append(out, p)
			}
		}
		trustedProxies.raw, trustedProxies.prefixes = raw, out
	}
	return trustedProxies.prefixes
}

// Forwarded addresses are accepted only from explicitly trusted immediate peers.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return ""
	}
	peer = peer.Unmap()
	prefixes := trustedPrefixes()
	trusted := func(ip netip.Addr) bool {
		for _, p := range prefixes {
			if p.Contains(ip) {
				return true
			}
		}
		return false
	}
	if !trusted(peer) {
		return peer.String()
	}
	chain := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	if len(chain) > 32 {
		return peer.String()
	}
	current := peer
	for i := len(chain) - 1; i >= 0; i-- {
		if !trusted(current) {
			break
		}
		ip, e := netip.ParseAddr(strings.TrimSpace(chain[i]))
		if e != nil {
			return peer.String()
		}
		current = ip.Unmap()
	}
	return current.String()
}

var clientField = regexp.MustCompile(`(?i)(?:^|[ ,])(?:Client|Version)="([^"\r\n]{0,128})"`)

func clientApp(r *http.Request) (string, string) {
	var name, version string
	for _, header := range []string{"X-Emby-Authorization", "X-MediaBrowser-Authorization", "Authorization"} {
		value := r.Header.Get(header)
		if len(value) > 8192 {
			continue
		}
		for _, m := range clientField.FindAllStringSubmatch(value, -1) {
			if strings.Contains(strings.ToLower(m[0]), "client=") {
				name = m[1]
			} else {
				version = m[1]
			}
		}
	}
	return name, version
}
func safeUA(r *http.Request) string {
	v := r.UserAgent()
	if len(v) > 512 {
		v = v[:512]
	}
	return strings.Map(func(c rune) rune {
		if c < 32 || c == 127 {
			return -1
		}
		return c
	}, v)
}
