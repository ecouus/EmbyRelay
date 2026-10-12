package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

type diagnosticKey struct{}

func WithDiagnostic(ctx context.Context, fn func(string)) context.Context {
	return context.WithValue(ctx, diagnosticKey{}, fn)
}
func report(ctx context.Context, reason string) {
	if fn, ok := ctx.Value(diagnosticKey{}).(func(string)); ok {
		fn(reason)
	}
}
func reportError(ctx context.Context, err error) {
	reason := "回源连接失败（检查源站地址、TLS 和网络）"
	var dns *net.DNSError
	var ne net.Error
	switch {
	case errors.As(err, &dns):
		reason = "回源 DNS 解析失败"
	case errors.As(err, &ne) && ne.Timeout():
		reason = "回源连接或响应超时"
	case errors.Is(err, context.Canceled):
		reason = "客户端取消请求"
	default:
		// Only fixed internal messages are included; never persist raw network errors or URLs.
		switch err.Error() {
		case "redirect target denied":
			reason = "回源重定向目标被拒绝：检查精确跳转域名白名单及 HTTPS 协议"
		case "redirect address denied":
			reason = "回源重定向 IP 被网络策略拒绝"
		case "API external redirect denied":
			reason = "回源 API 跨源重定向被拒绝"
		case "redirect limit exceeded":
			reason = "回源重定向次数超限"
		}
	}
	report(ctx, reason)
}
func reportStatus(ctx context.Context, status int) {
	if status == 401 {
		if trace, ok := ctx.Value(authKey{}).(*AuthTrace); ok {
			advice := "认证相关字段存在不代表凭据有效；检查 Emby Token 是否过期、账号授权及源站认证策略"
			code := "upstream_auth_rejected"
			if len(trace.Inbound) == 0 {
				code = "inbound_auth_missing"
				advice = "入站未发现常见认证字段；检查播放器是否给此请求携带 Emby Token"
			} else if missingAuthFields(trace.Inbound, trace.Outbound) {
				code = "outbound_auth_missing"
				advice = "部分入站认证字段在实际回源时缺失；检查跳转策略及客户端 Connection 头"
			}
			reason := "回源：源站返回 HTTP 401"
			if trace.Operation == "playback_progress" {
				reason = "回源：播放进度上报被源站拒绝（HTTP 401）"
				advice += "；可能影响观看进度和续播位置，视频流仍可能正常播放"
			}
			emitDetail(ctx, Detail{Code: code, Advice: advice})
			report(ctx, reason)
			return
		}
	}

	if status >= 400 {
		report(ctx, fmt.Sprintf("回源：源站返回 HTTP %d", status))
	}
}

type Detail struct {
	Auth   *AuthTrace `json:"auth,omitempty"`
	Target string     `json:"target"`
	Code   string     `json:"code"`
	Advice string     `json:"advice"`
}
type detailKey struct{}

func WithDetail(ctx context.Context, fn func(Detail)) context.Context {
	return context.WithValue(ctx, detailKey{}, fn)
}
func redirectDenied(ctx context.Context, target *url.URL, upstream *url.URL) error {
	d := Detail{Code: "redirect_invalid", Advice: "检查源站返回的跳转地址格式"}
	if target != nil && target.User == nil && target.Host != "" && (target.Scheme == "https" || target.Scheme == "http") {
		d.Target = target.Scheme + "://" + target.Host
		d.Code = "redirect_host_denied"
		d.Advice = "确认目标可信后，允许该域名并重新播放"
		if upstream.Scheme == "https" && target.Scheme != "https" {
			d.Code = "redirect_downgrade"
			d.Advice = "源站从 HTTPS 跳转到 HTTP，请修正源站；添加白名单不能解决"
		}
		if target.Fragment != "" {
			d.Code = "redirect_invalid"
			d.Advice = "源站跳转包含不支持的片段，请修正源站"
		}
	}
	emitDetail(ctx, d)
	return fmt.Errorf("redirect target denied")
}

// Only fixed field names and redirect origins are recorded, never credential values.
type AuthTrace struct {
	Operation string   `json:"operation"`
	Inbound   []string `json:"inbound_fields"`
	Outbound  []string `json:"outbound_fields"`
	Redirects []string `json:"redirect_origins"`
}
type authKey struct{}

func authFields(r *http.Request) []string {
	fields := []string{}
	for _, name := range []string{"Authorization", "X-Emby-Token", "X-Emby-Authorization", "X-MediaBrowser-Token", "X-MediaBrowser-Authorization", "Cookie"} {
		if strings.TrimSpace(r.Header.Get(name)) != "" {
			fields = append(fields, name)
		}
	}
	for _, name := range []string{"api_key", "X-Emby-Token", "access_token"} {
		if r.URL.Query().Get(name) != "" {
			fields = append(fields, "query:"+name)
		}
	}
	return fields
}
func emitDetail(ctx context.Context, d Detail) {
	d.Auth, _ = ctx.Value(authKey{}).(*AuthTrace)
	if fn, ok := ctx.Value(detailKey{}).(func(Detail)); ok {
		fn(d)
	}
}
func traceRequest(r *http.Request) *http.Request {
	trace := &AuthTrace{Inbound: authFields(r), Outbound: []string{}, Redirects: []string{}}
	if strings.EqualFold(strings.TrimSuffix(r.URL.Path, "/"), "/emby/Sessions/Playing/Progress") || strings.EqualFold(strings.TrimSuffix(r.URL.Path, "/"), "/Sessions/Playing/Progress") {
		trace.Operation = "playback_progress"
	}
	r = r.WithContext(context.WithValue(r.Context(), authKey{}, trace))
	emitDetail(r.Context(), Detail{})
	return r
}

func missingAuthFields(inbound, outbound []string) bool {
	for _, field := range inbound {
		found := false
		for _, other := range outbound {
			if field == other {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}
	return false
}
