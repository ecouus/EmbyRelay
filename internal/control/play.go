package control

import (
	"bufio"
	"context"
	"crypto/subtle"
	"embyrelay/internal/proxy"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

func (a *App) play(w http.ResponseWriter, r *http.Request) {
	state := a.state.Load()
	if state == nil {
		apiError(w, 503, "configuration unavailable")
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.EscapedPath(), "/r/"), "/", 3)
	if len(parts) < 2 {
		diagnosticReason(r, "本机：播放地址格式不完整")
		http.NotFound(w, r)
		return
	}
	rr := state.routes[parts[0]]
	if rr == nil {
		diagnosticReason(r, "本机：路由不存在或未启用")
		http.NotFound(w, r)
		return
	}
	digest := Hash(parts[1])
	valid := false
	var accessKey Key
	for _, key := range rr.keys {
		match := subtle.ConstantTimeCompare([]byte(digest), []byte(key.Hash)) == 1
		if match && key.Enabled && (key.ExpiresAt == 0 || key.ExpiresAt > time.Now().Unix()) {
			valid = true
			accessKey = key
		}
	}
	if rr.route.OwnerAccountID != "" && accessKey.AccountID != rr.route.OwnerAccountID {
		valid = false
	}
	if !valid {
		diagnosticReason(r, "本机：访问密钥无效或过期")
		apiError(w, 401, "invalid or expired access key")
		return
	}
	if !a.AgentMode && rr.route.RelayID != "" {
		if nodes := a.nodes.Load(); nodes != nil {
			var selected *Node
			for _, node := range *nodes {
				if (node.ID == rr.route.RelayID || rr.route.RelayID == "auto") && node.Enabled && (accessKey.AccountID == "" || (node.SubscriptionSupport && node.DeviceIdentitySupport)) && node.ConfigVersion >= state.snapshot.Version && time.Now().Unix()-node.LastSeen < 60 {
					if selected == nil || node.Active < selected.Active {
						copy := node
						selected = &copy
					}
				}
			}
			if selected != nil {
				w.Header().Set("Cache-Control", "no-store")
				http.Redirect(w, r, strings.TrimRight(selected.Endpoint, "/")+r.URL.RequestURI(), 307)
				return
			}
		}
	}
	if rr.usage.active.Add(1) > int64(rr.route.MaxConcurrent) {
		rr.usage.active.Add(-1)
		apiError(w, 503, "route at capacity")
		return
	}
	defer rr.usage.active.Add(-1)
	a.active.Add(1)
	a.requests.Add(1)
	defer a.active.Add(-1)
	if rr.route.DailyQuota > 0 {
		traffic, e := a.Store.Traffic()
		if e != nil {
			apiError(w, 503, "accounting unavailable")
			return
		}
		for _, t := range traffic {
			if t.RouteID == rr.route.ID && t.Date == time.Now().UTC().Format("2006-01-02") && t.Egress >= rr.route.DailyQuota {
				apiError(w, 429, "daily quota exhausted")
				return
			}
		}
	}
	session := ID()
	clientName, clientVersion := clientApp(r)
	if _, e := a.Store.DB.Exec("INSERT INTO stream_sessions(id,route_id,started_at,status,client_ip,user_agent,client_name,client_version) VALUES(?,?,?,'active',?,?,?,?)", session, rr.route.ID, time.Now().Unix(), clientIP(r), safeUA(r), clientName, clientVersion); e != nil {
		apiError(w, 503, "accounting unavailable")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	mw := &meterWriter{ResponseWriter: w, app: a, route: rr.route, usage: rr.usage, ctx: ctx, session: session, status: "completed"}
	completed := false
	defer func() {
		mw.flushIngress()
		mw.mu.Lock()
		defer mw.mu.Unlock()
		status := mw.status
		if !completed {
			status = "interrupted"
		}
		a.Store.DB.Exec("UPDATE stream_sessions SET ended_at=?,bytes_sent=?,status=? WHERE id=?", time.Now().Unix(), mw.bytes, status, session)
	}()
	prefix := "/r/" + parts[0] + "/" + parts[1]
	q := r.Clone(ctx)
	q.URL.RawPath = normalizePlaybackPath(r.URL.EscapedPath(), prefix)
	q.URL.Path, _ = url.PathUnescape(q.URL.RawPath)
	if q.URL.Path == "" {
		q.URL.Path = "/"
		q.URL.RawPath = "/"
	}
	q.RequestURI = ""
	q.Header.Del("Cookie")
	for _, cookie := range r.Cookies() {
		if cookie.Name != "relay_session" && cookie.Name != "relay_portal" && (cookie.Name != subscriptionCookie || accessKey.AccountID != "") {
			q.AddCookie(cookie)
		}
	}
	if r.ContentLength > 32*1024*1024 {
		apiError(w, 413, "request body too large")
		return
	}
	q.Body = &deadlineBody{ReadCloser: http.MaxBytesReader(w, r.Body, 32*1024*1024), controller: http.NewResponseController(w)}
	if d, ok := r.Context().Value(detailKey{}).(*diagnosticDetail); ok {
		d.Route = rr.route.Alias
		d.UpstreamID = rr.route.UpstreamID
		d.UpstreamPath = diagnosticPath(q)
	}
	gateway := rr.gateway
	if accessKey.AccountID != "" {
		var done func()
		var handled bool
		q, gateway, done, handled = a.prepareAccess(mw, q, rr, accessKey, mw, prefix, cancel)
		defer done()
		if handled {
			completed = true
			return
		}
	}
	q = q.WithContext(proxy.WithRequestOptions(q.Context(), prefix, mw.addIngress))
	diagnosticReason(r, "回源阶段：源站 HTTP 错误或代理连接失败")
	q = q.WithContext(proxy.WithDiagnostic(q.Context(), func(reason string) { diagnosticReason(r, reason) }))
	if d, ok := r.Context().Value(detailKey{}).(*diagnosticDetail); ok {
		d.Route = rr.route.Alias
		d.UpstreamID = rr.route.UpstreamID
		d.UpstreamPath = diagnosticPath(q)
		q = q.WithContext(proxy.WithDetail(q.Context(), func(v proxy.Detail) { d.Detail = v }))
	}
	gateway.ServeHTTP(mw, q)
	completed = true
}

type meterWriter struct {
	responseStatus int
	accessCharge   func(int) error
	accessRefund   int64
	stream         bool
	connection     net.Conn
	mu             sync.Mutex
	http.ResponseWriter
	app            *App
	route          Route
	usage          *routeRuntime
	ctx            context.Context
	session        string
	bytes          int64
	pendingIngress int64
	lastProgress   time.Time
	status         string
}

func (w *meterWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *meterWriter) WriteHeader(code int) {
	w.mu.Lock()
	if w.responseStatus == 0 && code >= 200 {
		w.responseStatus = code
	}
	w.mu.Unlock()
	if code >= 400 {
		w.setStatus("http_error")
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *meterWriter) wait(n int) error {
	if w.route.RateBPS <= 0 {
		return nil
	}
	w.usage.rateMu.Lock()
	now := time.Now()
	start := w.usage.next
	if start.Before(now) {
		start = now
	}
	delay := time.Duration(float64(n) / float64(w.route.RateBPS) * float64(time.Second))
	w.usage.next = start.Add(delay)
	w.usage.rateMu.Unlock()
	t := time.NewTimer(time.Until(start.Add(delay)))
	defer t.Stop()
	select {
	case <-w.ctx.Done():
		return w.ctx.Err()
	case <-t.C:
		return nil
	}
}
func (w *meterWriter) reserve(n int) error {
	if w.accessCharge != nil {
		if err := w.accessCharge(n); err != nil {
			return err
		}
	}
	if e := w.wait(n); e != nil {
		return e
	}
	w.mu.Lock()
	ingress := w.pendingIngress
	w.pendingIngress = 0
	w.mu.Unlock()
	e := w.app.Store.Charge(w.route.ID, w.route.DailyQuota, ingress, int64(n))
	if errors.Is(e, ErrQuota) && w.app.AcquireQuota != nil && w.route.DailyQuota > 0 {
		if e = w.app.AcquireQuota(w.ctx, w.route, int64(n)); e == nil {
			e = w.app.Store.Charge(w.route.ID, w.route.DailyQuota, ingress, int64(n))
		}
	}
	if e != nil {
		w.mu.Lock()
		w.pendingIngress += ingress
		w.mu.Unlock()
		w.setStatus("quota_or_accounting_error")
		return e
	}
	return nil
}

// Merge ingress accounting with the next egress reservation instead of
// opening a second transaction on every upstream body read.
func (w *meterWriter) addIngress(n int) {
	w.mu.Lock()
	w.pendingIngress += int64(n)
	w.mu.Unlock()
}
func (w *meterWriter) flushIngress() {
	w.mu.Lock()
	n := w.pendingIngress
	w.pendingIngress = 0
	w.mu.Unlock()
	if n > 0 && w.app.Store.Charge(w.route.ID, 0, n, 0) != nil {
		w.setStatus("quota_or_accounting_error")
	}
}
func (w *meterWriter) record(n, reserved int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.bytes += int64(n)
	if n < reserved {
		w.app.Store.Charge(w.route.ID, 0, 0, int64(n-reserved))
		if w.accessCharge != nil {
			w.accessRefund += int64(reserved - n)
		}
	}
	if time.Since(w.lastProgress) >= time.Second {
		w.app.Store.DB.Exec("UPDATE stream_sessions SET bytes_sent=? WHERE id=?", w.bytes, w.session)
		w.lastProgress = time.Now()
	}
}
func (w *meterWriter) Write(b []byte) (int, error) {
	total := 0
	for len(b) > 0 {
		size := len(b)
		chunk := 32 * 1024
		if w.stream {
			chunk = 256 * 1024
		}
		if size > chunk {
			size = chunk
		}
		for {
			if e := w.reserve(size); e != nil {
				if w.stream && size > 1 && e.Error() == "本周期剩余流量不足" {
					size /= 2
					continue
				}
				w.setStatus("subscription_denied")
				panic(http.ErrAbortHandler)
			}
			break
		}
		http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(30 * time.Second))
		n, e := w.ResponseWriter.Write(b[:size])
		http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Time{})
		w.record(n, size)
		total += n
		if e != nil {
			return total, e
		}
		if n != size {
			return total, io.ErrShortWrite
		}
		b = b[size:]
	}
	return total, nil
}
func (w *meterWriter) Flush() {
	controller := http.NewResponseController(w.ResponseWriter)
	controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	defer controller.SetWriteDeadline(time.Time{})
	if e := http.NewResponseController(w.ResponseWriter).Flush(); e != nil {
		w.setStatus("write_error")
	}
}
func (w *meterWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijacking unsupported")
	}
	c, b, e := h.Hijack()
	if e != nil {
		return nil, nil, e
	}
	w.mu.Lock()
	w.connection = c
	w.mu.Unlock()
	return &meterConn{Conn: c, w: w}, b, nil
}

type meterConn struct {
	net.Conn
	w *meterWriter
}

func (c *meterConn) Write(b []byte) (int, error) {
	if e := c.w.reserve(len(b)); e != nil {
		c.w.setStatus("subscription_denied")
		c.Close()
		return 0, e
	}
	c.Conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	n, e := c.Conn.Write(b)
	c.w.record(n, len(b))
	return n, e
}

func (w *meterWriter) setStatus(s string) { w.mu.Lock(); defer w.mu.Unlock(); w.status = s }

type deadlineBody struct {
	io.ReadCloser
	controller *http.ResponseController
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	b.controller.SetReadDeadline(time.Now().Add(30 * time.Second))
	n, e := b.ReadCloser.Read(p)
	b.controller.SetReadDeadline(time.Time{})
	return n, e
}

// Normalize only repeated prefixes carrying the already authenticated route/key.
// Some players append a root-relative PlaybackInfo URL to their /emby base.
func normalizePlaybackPath(path, prefix string) string {
	path = strings.TrimPrefix(path, prefix)
	// Clients can join a base URL ending in / with an /emby API path.
	// Normalize only leading separators; preserve escaped and interior paths.
	path = "/" + strings.TrimLeft(path, "/")
	for {
		if strings.HasPrefix(path, prefix+"/") {
			path = strings.TrimPrefix(path, prefix)
		} else if strings.HasPrefix(path, "/emby"+prefix+"/") {
			path = "/emby" + strings.TrimPrefix(path, "/emby"+prefix)
		} else {
			break
		}
	}
	return "/" + strings.TrimLeft(path, "/")
}
