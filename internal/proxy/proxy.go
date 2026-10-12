package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	AutoMediaRedirect bool
	Upstream          *url.URL
	AccessKey         string
	Network           NetworkPolicy
	MaxConcurrent     int
	MaxResumes        int
	ReadTimeout       time.Duration
	Mode              string
	RedirectHosts     []string
	TrustedAuth       bool
}
type Gateway struct {
	cfg       Config
	transport http.RoundTripper
	api       *httputil.ReverseProxy
	slots     chan struct{}
	key       [32]byte
}

// New creates separate API and media connection pools with network-policy
// validation and response rewriting for relay paths.
func New(cfg Config) *Gateway {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 100
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = 30 * time.Second
	}
	newTransport := func() *http.Transport {
		return &http.Transport{DialContext: cfg.Network.DialContext, ForceAttemptHTTP2: true, MaxIdleConns: 128, MaxIdleConnsPerHost: 32, MaxConnsPerHost: cfg.MaxConcurrent, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 15 * time.Second, DisableCompression: true}
	}
	g := &Gateway{cfg: cfg, transport: &policyTransport{base: newTransport(), cfg: cfg}, slots: make(chan struct{}, cfg.MaxConcurrent), key: sha256.Sum256([]byte(cfg.AccessKey))}
	g.api = &httputil.ReverseProxy{Transport: &policyTransport{base: newTransport(), cfg: cfg}, Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(cfg.Upstream)
		pr.Out.Header.Del("X-Relay-Key")
		pr.Out.Header.Del("Referer")
		pr.Out.Header.Set("Accept-Encoding", "identity")
		pr.Out.Header.Del("Forwarded")
		pr.Out.Header.Del("X-Forwarded-For")
		pr.Out.Header.Del("X-Forwarded-Host")
		pr.Out.Header.Del("X-Forwarded-Proto")
	}, ModifyResponse: g.modifyResponse, ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
		reportError(r.Context(), err)
		http.Error(w, "upstream unavailable", 502)
	}}
	return g
}
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = traceRequest(r)
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.URL.Path == "/healthz" {
		w.WriteHeader(200)
		return
	}
	key := sha256.Sum256([]byte(r.Header.Get("X-Relay-Key")))
	if !g.cfg.TrustedAuth && subtle.ConstantTimeCompare(key[:], g.key[:]) != 1 {
		http.Error(w, "unauthorized", 401)
		return
	}
	select {
	case g.slots <- struct{}{}:
		defer func() { <-g.slots }()
	default:
		http.Error(w, "capacity exceeded", 503)
		return
	}
	// Static Emby video and audio endpoints only. Other requests use the standard
	// proxy, including WebSocket upgrades and POST bodies, without application retries.
	if _, ok := r.Context().Value(accessProtectionKey{}).(accessProtection); ok {
		g.api.ServeHTTP(w, r)
		return
	}
	p := strings.ToLower(r.URL.Path)
	if r.Method == http.MethodGet && (strings.HasPrefix(p, "/videos/") || strings.HasPrefix(p, "/audio/") || strings.HasPrefix(p, "/emby/videos/") || strings.HasPrefix(p, "/emby/audio/")) && (strings.HasSuffix(p, "/stream") || strings.Contains(p, "/stream.")) && !strings.Contains(r.Header.Get("Range"), ",") {
		g.stream(w, r)
		return
	}
	g.api.ServeHTTP(w, r)
}
func stripHop(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, k := range strings.Split(v, ",") {
			h.Del(strings.TrimSpace(k))
		}
	}
	for _, k := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "X-Relay-Key", "Referer", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
		h.Del(k)
	}
}
func (g *Gateway) request(r *http.Request) *http.Request {
	q := r.Clone(r.Context())
	u := *r.URL
	q.URL = &u
	q.URL.Scheme = g.cfg.Upstream.Scheme
	q.URL.Host = g.cfg.Upstream.Host
	q.URL.Path = strings.TrimRight(g.cfg.Upstream.Path, "/") + r.URL.Path
	q.URL.RawPath = strings.TrimRight(g.cfg.Upstream.EscapedPath(), "/") + r.URL.EscapedPath()
	q.RequestURI = ""
	q.Host = g.cfg.Upstream.Host
	stripHop(q.Header)
	q.Header.Set("Accept-Encoding", "identity")
	return q
}

// rangeInfo uses an exclusive end and rejects unknown totals and overflow.
func rangeInfo(v string) (start, end, total int64, err error) {
	err = fmt.Errorf("invalid content range")
	if !strings.HasPrefix(v, "bytes ") {
		return
	}
	parts := strings.Split(strings.TrimPrefix(v, "bytes "), "/")
	if len(parts) != 2 {
		return
	}
	bounds := strings.Split(parts[0], "-")
	if len(bounds) != 2 {
		return
	}
	a, e1 := strconv.ParseInt(bounds[0], 10, 64)
	b, e2 := strconv.ParseInt(bounds[1], 10, 64)
	t, e3 := strconv.ParseInt(parts[1], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil || a < 0 || b < a || t <= b {
		return
	}
	return a, b + 1, t, nil
}
func strongETag(v string) bool { return len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' }

// timedRead measures only blocked upstream reads, not time spent writing to a
// slow downstream. Cancelling the request unblocks net/http body reads.
func timedRead(body io.Reader, b []byte, cancel context.CancelFunc, timeout time.Duration) (int, error) {
	timer := time.AfterFunc(timeout, cancel)
	defer timer.Stop()
	return body.Read(b)
}
func (g *Gateway) stream(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	attemptCtx, attemptCancel := context.WithCancel(ctx)
	defer func() { attemptCancel() }()
	req := g.request(r.WithContext(attemptCtx))
	resp, err := g.mediaRoundTrip(req)
	if err != nil {
		reportError(r.Context(), err)
		http.Error(w, "upstream unavailable", 502)
		return
	}
	defer func() { resp.Body.Close() }()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.StatusCode != 304 && g.cfg.Mode != "redirect" && g.cfg.Mode != "auto" {
		report(r.Context(), "回源媒体重定向被拒绝")
		http.Error(w, "upstream redirect denied", 502)
		return
	}
	start, end, total := int64(0), resp.ContentLength, resp.ContentLength
	if resp.StatusCode == 206 {
		start, end, total, err = rangeInfo(resp.Header.Get("Content-Range"))
		if err != nil || !matchesRange(r.Header.Get("Range"), start, end, total) || (resp.ContentLength >= 0 && resp.ContentLength != end-start) {
			report(r.Context(), "回源 Range 响应不合法或与请求不一致")
			http.Error(w, "invalid upstream range", 502)
			return
		}
	}
	tag := resp.Header.Get("ETag")
	recoverable := (resp.StatusCode == 200 || resp.StatusCode == 206) && end > start && strongETag(tag) && resp.Header.Get("Content-Encoding") == "" && !strings.HasPrefix(resp.Header.Get("Content-Type"), "multipart/")
	stripHop(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	reportStatus(r.Context(), resp.StatusCode)
	w.WriteHeader(resp.StatusCode)
	next := start
	buf := make([]byte, 256*1024)
	attempts := 0
	failedProgress := 0
	lastProgress := start
	var resumeTimes []time.Time
	for {
		n, readErr := timedRead(resp.Body, buf, attemptCancel, g.cfg.ReadTimeout)
		if n > 0 {
			if end >= 0 && (resp.StatusCode == 200 || resp.StatusCode == 206) && int64(n) > end-next {
				panic(http.ErrAbortHandler)
			}
			written, writeErr := w.Write(buf[:n])
			next += int64(written)
			if writeErr != nil || written != n {
				return
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				return
			}
		}
		if end >= 0 && next == end && (resp.StatusCode == 200 || resp.StatusCode == 206) {
			return
		}
		if readErr == nil {
			continue
		}
		if readErr == io.EOF && (!recoverable || end < 0) {
			if end < 0 || next == end {
				return
			}
		}
		if !recoverable || attempts >= g.cfg.MaxResumes || ctx.Err() != nil {
			slog.Warn("stream terminated", "bytes_sent", next-start, "resumes", attempts)
			panic(http.ErrAbortHandler)
		}
		now := time.Now()
		recent := resumeTimes[:0]
		for _, ts := range resumeTimes {
			if now.Sub(ts) < time.Minute {
				recent = append(recent, ts)
			}
		}
		resumeTimes = recent
		if next-lastProgress < 512*1024 {
			failedProgress++
		} else {
			failedProgress = 0
		}
		lastProgress = next
		if failedProgress > 3 || len(resumeTimes) >= 8 {
			panic(http.ErrAbortHandler)
		}
		resumeTimes = append(resumeTimes, now)
		attempts++
		resp.Body.Close()
		attemptCancel()
		select {
		case <-ctx.Done():
			panic(http.ErrAbortHandler)
		case <-time.After(time.Duration(attempts) * 100 * time.Millisecond):
		}
		attemptCtx, attemptCancel = context.WithCancel(ctx)
		q := req.Clone(attemptCtx)
		q.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", next, end-1))
		q.Header.Set("If-Range", tag)
		recovered, e := g.mediaRoundTrip(q)
		if e != nil {
			panic(http.ErrAbortHandler)
		}
		a, b, t, rangeErr := rangeInfo(recovered.Header.Get("Content-Range"))
		if recovered.StatusCode != 206 || rangeErr != nil || a != next || b != end || t != total || recovered.Header.Get("ETag") != tag || recovered.Header.Get("Content-Encoding") != "" || (recovered.ContentLength >= 0 && recovered.ContentLength != end-next) {
			recovered.Body.Close()
			panic(http.ErrAbortHandler)
		}
		resp = recovered
	}
}

// matchesRange validates the original single-range response before committing it.
func matchesRange(v string, start, end, total int64) bool {
	if !strings.HasPrefix(v, "bytes=") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(v, "bytes="), "-")
	if len(parts) != 2 {
		return false
	}
	if parts[0] == "" {
		suffix, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffix <= 0 {
			return false
		}
		if suffix > total {
			suffix = total
		}
		return start == total-suffix && end == total
	}
	first, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || first < 0 || first != start {
		return false
	}
	if parts[1] == "" {
		return end == total
	}
	last, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || last < first {
		return false
	}
	if last >= total {
		last = total - 1
	}
	return end == last+1
}

// mediaRoundTrip retries safe media GETs only before their bytes are forwarded.
func (g *Gateway) mediaRoundTrip(r *http.Request) (*http.Response, error) {
	for attempt := 0; attempt < 3; attempt++ {
		resp, e := g.transport.RoundTrip(r)
		retry := e != nil || (resp != nil && (resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504))
		if !retry || attempt == 2 || r.Context().Err() != nil {
			return resp, e
		}
		if resp != nil {
			resp.Body.Close()
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
		select {
		case <-r.Context().Done():
			timer.Stop()
			return nil, r.Context().Err()
		case <-timer.C:
		}
	}
	return nil, fmt.Errorf("retry budget exhausted")
}
