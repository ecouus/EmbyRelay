package control

import (
	"bufio"
	"context"
	"embyrelay/internal/proxy"
	"encoding/json"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

type diagnosticKey struct{}
type detailKey struct{}
type diagnosticDetail struct {
	DeviceIdentity *DeviceIdentity `json:"device_identity,omitempty"`
	proxy.Detail
	Route        string `json:"route"`
	UpstreamID   string `json:"upstream_id"`
	UpstreamPath string `json:"upstream_path"`
	DurationMS   int64  `json:"duration_ms"`
}

type diagnosticWriter struct {
	http.ResponseWriter
	status int
}

func (w *diagnosticWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *diagnosticWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *diagnosticWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(b)
}
func (w *diagnosticWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *diagnosticWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}
func diagnosticReason(r *http.Request, reason string) {
	if v, ok := r.Context().Value(diagnosticKey{}).(*string); ok {
		*v = reason
	}
}

var accessPath = regexp.MustCompile(`(/r/[^/]+/)[^/?]+`)

func diagnosticPath(r *http.Request) string {
	return accessPath.ReplaceAllString(r.URL.EscapedPath(), `${1}***`)
}
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reason := "本机请求未匹配或被拒绝"
	detail := &diagnosticDetail{}
	started := time.Now()
	id := ID()
	w.Header().Set("X-Request-ID", id)
	dw := &diagnosticWriter{ResponseWriter: w}
	defer func() {
		if dw.status < 400 {
			return
		}
		detail.DurationMS = time.Since(started).Milliseconds()
		encoded, _ := json.Marshal(detail)
		source := "local"
		if strings.HasPrefix(reason, "回源") {
			source = "upstream"
		}
		a.Store.DB.Exec("INSERT INTO request_diagnostics(id,status,reason,created_at,method,path,source,details) VALUES(?,?,?,?,?,?,?,?)", id, dw.status, reason, time.Now().Unix(), r.Method, diagnosticPath(r), source, string(encoded))
		a.Store.DB.Exec("DELETE FROM request_diagnostics WHERE id NOT IN (SELECT id FROM request_diagnostics ORDER BY created_at DESC, rowid DESC LIMIT 1000)")
	}()
	a.serveHTTP(dw, r.WithContext(context.WithValue(context.WithValue(r.Context(), diagnosticKey{}, &reason), detailKey{}, detail)))
}
