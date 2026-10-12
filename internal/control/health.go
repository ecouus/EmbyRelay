package control

import (
	"context"
	"embyrelay/internal/proxy"
	"net/http"
	"net/netip"
	"time"
)

func (a *App) probe(w http.ResponseWriter, r *http.Request, id string) {
	u, e := get[Upstream](a.Store, "upstream", id)
	if e != nil {
		apiError(w, 404, "upstream not found")
		return
	}
	target, e := proxy.ValidateURL(u.BaseURL)
	if e != nil {
		apiError(w, 400, "invalid upstream")
		return
	}
	target.Path += "/System/Info/Public"
	p := proxy.NetworkPolicy{}
	for _, cidr := range u.AllowedCIDRs {
		v, e := netip.ParsePrefix(cidr)
		if e != nil {
			apiError(w, 400, "invalid network policy")
			return
		}
		p.Allowed = append(p.Allowed, v)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	transport := &http.Transport{DialContext: p.DialContext, ResponseHeaderTimeout: 4 * time.Second, TLSHandshakeTimeout: 4 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(ctx, "GET", target.String(), nil)
	start := time.Now()
	resp, e := client.Do(req)
	status := 0
	if e == nil {
		status = resp.StatusCode
		resp.Body.Close()
	}
	writeJSON(w, 200, map[string]any{"reachable": e == nil, "healthy": e == nil && status >= 200 && status < 300, "status_code": status, "latency_ms": time.Since(start).Milliseconds()})
}
