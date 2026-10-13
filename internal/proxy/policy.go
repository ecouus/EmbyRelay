package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

type requestOptions struct {
	prefix  string
	ingress func(int)
}
type optionsKey struct{}

func WithRequestOptions(ctx context.Context, prefix string, ingress func(int)) context.Context {
	return context.WithValue(ctx, optionsKey{}, requestOptions{prefix, ingress})
}
func options(ctx context.Context) requestOptions {
	o, _ := ctx.Value(optionsKey{}).(requestOptions)
	return o
}

type observeBody struct {
	io.ReadCloser
	fn func(int)
}

func (b *observeBody) Read(p []byte) (int, error) {
	n, e := b.ReadCloser.Read(p)
	if n > 0 {
		b.fn(n)
	}
	return n, e
}

func (b *observeBody) Write(p []byte) (int, error) {
	if w, ok := b.ReadCloser.(io.Writer); ok {
		return w.Write(p)
	}
	return 0, fmt.Errorf("body is not writable")
}

type policyTransport struct {
	base *http.Transport
	cfg  Config
}

func (t *policyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	current := r
	for hop := 0; hop < 6; hop++ {
		if trace, ok := r.Context().Value(authKey{}).(*AuthTrace); ok {
			trace.Outbound = authFields(current)
		}
		resp, e := t.base.RoundTrip(current)
		if e != nil {
			return nil, e
		}
		if fn := options(r.Context()).ingress; fn != nil {
			resp.Body = &observeBody{resp.Body, fn}
		}
		if resp.StatusCode < 300 || resp.StatusCode >= 400 || resp.StatusCode == 304 {
			return resp, nil
		}
		target, e := current.URL.Parse(resp.Header.Get("Location"))
		if e == nil && target.User == nil && target.Host != "" && (target.Scheme == "https" || target.Scheme == "http") {
			if trace, ok := r.Context().Value(authKey{}).(*AuthTrace); ok {
				trace.Redirects = append(trace.Redirects, target.Scheme+"://"+target.Host)
			}
		}
		forced, _ := r.Context().Value(forceMediaKey{}).(bool)
		media := (forced || isMedia(r.URL.Path)) && (r.Method == "GET" || r.Method == "HEAD")
		if e != nil || !t.allowed(target, media) {
			resp.Body.Close()
			return nil, redirectDenied(r.Context(), target, t.cfg.Upstream)
		}
		if e = t.cfg.Network.CheckURL(r.Context(), target); e != nil {
			resp.Body.Close()
			return nil, e
		}
		if media && !t.cfg.AutoMediaRedirect && (t.cfg.Mode == "redirect" || t.cfg.Mode == "auto") && !sameOrigin(target, t.cfg.Upstream) {
			resp.Header.Set("Location", target.String())
			return resp, nil
		}
		if !media {
			if !sameOrigin(target, t.cfg.Upstream) {
				resp.Body.Close()
				return nil, fmt.Errorf("API external redirect denied")
			}
			resp.Header.Set("Location", localURL(target, options(r.Context()).prefix, t.cfg.Upstream))
			return resp, nil
		}
		resp.Body.Close()
		next := current.Clone(r.Context())
		next.URL = target
		next.Host = target.Host
		if !sameOrigin(target, current.URL) {
			for _, k := range []string{"Authorization", "Cookie", "X-Emby-Token", "X-Emby-Authorization", "X-MediaBrowser-Token", "X-MediaBrowser-Authorization", "X-Relay-Key", "X-Emby-Device-Id", "X-Emby-Device-Name", "X-Emby-Client", "X-Emby-Client-Version"} {
				next.Header.Del(k)
			}
		}
		current = next
	}
	return nil, fmt.Errorf("redirect limit exceeded")
}
func (t *policyTransport) allowed(u *url.URL, media bool) bool {
	if u == nil || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	if media && t.cfg.AutoMediaRedirect {
		return true
	}
	if t.cfg.Upstream.Scheme == "https" && u.Scheme != "https" {
		return false
	}
	if sameOrigin(u, t.cfg.Upstream) {
		return true
	}
	for _, h := range t.cfg.RedirectHosts {
		if strings.EqualFold(u.Host, h) {
			return true
		}
	}
	return false
}
func (p NetworkPolicy) CheckURL(ctx context.Context, u *url.URL) error {
	ips, e := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname())
	if e != nil {
		return e
	}
	if len(ips) == 0 {
		return fmt.Errorf("empty DNS result")
	}
	for _, ip := range ips {
		if !p.permits(ip) {
			return fmt.Errorf("redirect address denied")
		}
	}
	return nil
}
func isMedia(path string) bool {
	p := strings.ToLower(path)
	return (strings.HasPrefix(p, "/videos/") || strings.HasPrefix(p, "/audio/") || strings.HasPrefix(p, "/emby/videos/") || strings.HasPrefix(p, "/emby/audio/")) && (strings.HasSuffix(p, "/stream") || strings.Contains(p, "/stream.") || strings.HasSuffix(p, "/original") || strings.Contains(p, "/original."))
}
func localURL(u *url.URL, prefix string, upstream *url.URL) string {
	out := *u
	out.Scheme = ""
	out.Host = ""
	// PlaybackInfo URLs may already have been rewritten by an earlier response;
	// do not prepend the relay prefix a second time.
	if strings.HasPrefix(out.Path, prefix+"/") || out.Path == prefix {
		return out.String()
	}
	out.Path = prefix + strings.TrimPrefix(out.Path, upstream.Path)
	out.RawPath = ""
	return out.String()
}
func (g *Gateway) modifyResponse(resp *http.Response) error {
	reportStatus(resp.Request.Context(), resp.StatusCode)
	prefix := options(resp.Request.Context()).prefix
	resp.Header.Set("Referrer-Policy", "no-referrer")
	resp.Header.Set("Content-Security-Policy", PlaybackCSP)
	resp.Header.Set("X-Content-Type-Options", "nosniff")
	if p, ok := resp.Request.Context().Value(accessProtectionKey{}).(accessProtection); ok {
		return protectAccessResponse(resp, prefix, p)
	}
	if prefix == "" {
		return nil
	}
	if cookies := resp.Header.Values("Set-Cookie"); len(cookies) > 0 {
		resp.Header.Del("Set-Cookie")
		for _, raw := range cookies {
			c, e := http.ParseSetCookie(raw)
			if e != nil {
				continue
			}
			c.Domain = ""
			c.Path = prefix + "/"
			resp.Header.Add("Set-Cookie", c.String())
		}
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") || resp.Header.Get("Content-Encoding") != "" {
		return nil
	}
	// Limit rewriting memory. Oversized responses are passed through intact.
	body, e := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024+1))
	if e != nil {
		return e
	}
	if len(body) > 8*1024*1024 {
		resp.Body = &joinedBody{Reader: io.MultiReader(bytes.NewReader(body), resp.Body), Closer: resp.Body}
		return nil
	}
	resp.Body.Close()
	var data any
	if json.Unmarshal(body, &data) == nil {
		rewriteURLs(data, prefix, g.cfg.Upstream)
		body, _ = json.Marshal(data)
		resp.Header.Del("ETag")
		resp.Header.Del("Content-MD5")
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", fmt.Sprint(len(body)))
	return nil
}

type joinedBody struct {
	io.Reader
	io.Closer
}

func rewriteURLs(v any, prefix string, upstream *url.URL) {
	switch x := v.(type) {
	case []any:
		for _, v := range x {
			rewriteURLs(v, prefix, upstream)
		}
	case map[string]any:
		for k, v := range x {
			if s, ok := v.(string); ok {
				switch strings.ToLower(k) {
				case "directstreamurl", "transcodingurl", "deliveryurl", "serveraddress", "localaddress", "wanaddress":
					u, e := url.Parse(s)
					if e == nil && ((u.Host == upstream.Host && u.Scheme == upstream.Scheme) || (u.Host == "" && strings.HasPrefix(u.Path, "/"))) {
						x[k] = localURL(u, prefix, upstream)
					}
				}
			} else {
				rewriteURLs(v, prefix, upstream)
			}
		}
	}
}
func (g *Gateway) CloseIdleConnections() {
	if t, ok := g.transport.(*policyTransport); ok {
		t.base.CloseIdleConnections()
	}
	if t, ok := g.api.Transport.(*policyTransport); ok {
		t.base.CloseIdleConnections()
	}
}

// Explicit default ports and omitted default ports identify the same origin.
func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return a.Scheme == b.Scheme && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}
