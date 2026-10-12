package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"embyrelay/internal/proxy"
)

var tokenAttribute = regexp.MustCompile(`(?i)Token\s*=\s*"([^"]*)"`)

func accessToken(r *http.Request) string {
	values := []string{r.Header.Get("X-Emby-Token"), r.Header.Get("X-MediaBrowser-Token")}
	for k, vs := range r.URL.Query() {
		if strings.EqualFold(k, "api_key") || strings.EqualFold(k, "X-Emby-Token") || strings.EqualFold(k, "access_token") {
			values = append(values, vs...)
		}
	}
	for _, name := range []string{"Authorization", "X-Emby-Authorization", "X-MediaBrowser-Authorization"} {
		v := r.Header.Get(name)
		for _, m := range tokenAttribute.FindAllStringSubmatch(v, -1) {
			values = append(values, m[1])
		}
		if strings.HasPrefix(v, "Bearer ") {
			values = append(values, strings.TrimPrefix(v, "Bearer "))
		}
	}
	token := ""
	for _, v := range values {
		if v != "" {
			if token != "" && token != v {
				return ""
			}
			token = v
		}
	}
	return token
}
func replaceAccessAuth(r *http.Request, source string) {
	q := r.URL.Query()
	for k := range q {
		if strings.EqualFold(k, "api_key") || strings.EqualFold(k, "X-Emby-Token") || strings.EqualFold(k, "access_token") {
			q.Del(k)
		}
	}
	r.URL.RawQuery = q.Encode()
	for _, k := range []string{"Authorization", "X-Emby-Authorization", "X-MediaBrowser-Authorization", "X-Emby-Token", "X-MediaBrowser-Token", "Cookie"} {
		r.Header.Del(k)
	}
	if source != "" {
		r.Header.Set("X-Emby-Token", source)
	}
}
func mediaRequest(r *http.Request) bool {
	p := strings.ToLower(r.URL.Path)
	return (r.Method == "GET" || r.Method == "HEAD") && (strings.Contains(p, "/videos/") || strings.Contains(p, "/audio/") || strings.Contains(p, "/__access_media/")) && !strings.Contains(p, "/images/") && !strings.Contains(p, "/subtitles/")
}
func (a *App) prepareAccess(w http.ResponseWriter, q *http.Request, rr *runtimeRoute, key Key, mw *meterWriter, prefix string, cancel context.CancelFunc) (*http.Request, *proxy.Gateway, func(), bool) {
	done := func() {}
	path := strings.ToLower(q.URL.Path)
	if strings.HasPrefix(path, "/emby/") {
		path = strings.TrimPrefix(path, "/emby")
	}
	command := AccessCommand{AccountID: key.AccountID, KeyID: key.ID, Token: accessToken(q)}
	if path == "/users/public" && q.Method == "GET" {
		writeJSON(w, 200, []any{})
		return q, nil, done, true
	}
	if path == "/users/authenticatebyname" && q.Method == "POST" {
		var in struct {
			Username string
			Pw       string
			Password string
		}
		if json.NewDecoder(http.MaxBytesReader(w, q.Body, 8192)).Decode(&in) != nil {
			apiError(w, 400, "invalid login")
			return q, nil, done, true
		}
		command.Action = "login"
		command.Username = in.Username
		command.Password = in.Pw
		if command.Password == "" {
			command.Password = in.Password
		}
		device := deviceRequestFields(q)
		command.Device, command.DeviceName, command.ClientName, command.ClientVersion = device.ID, device.Name, device.Client, device.Version
		if command.Device == "" {
			command.Device = safeUA(q)
		}
		command.ClientIP = clientIP(q)
		command.UserAgent = safeUA(q)
		out, e := a.accessCall(q.Context(), command)
		if e != nil {
			diagnosticReason(q, "订阅登录被拒绝")
			apiError(w, 401, e.Error())
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.Write(out.Payload)
		}
		return q, nil, done, true
	}
	if (path == "/system/info/public" || path == "/branding/configuration") && q.Method == "GET" {
		replaceAccessAuth(q, "")
		return q, rr.gateway, done, false
	}
	if command.Token == "" {
		diagnosticReason(q, "订阅：缺少代理登录 Token")
		apiError(w, 401, "请使用分配的账号登录")
		return q, nil, done, true
	}
	if strings.Contains(path, "/password") || strings.HasPrefix(path, "/auth/") {
		apiError(w, 403, "源站账号管理不可通过订阅地址操作")
		return q, nil, done, true
	}
	if path == "/sessions/logout" && q.Method == "POST" {
		command.Action = "logout"
		_, e := a.accessCall(q.Context(), command)
		if e != nil {
			apiError(w, 401, e.Error())
		} else {
			w.WriteHeader(204)
		}
		return q, nil, done, true
	}
	if path == "/subscription/info" && q.Method == "GET" {
		command.Action = "info"
		out, e := a.accessCall(q.Context(), command)
		if e != nil {
			apiError(w, 403, e.Error())
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.Write(out.Payload)
		}
		return q, nil, done, true
	}
	command.Action = "begin"
	command.Stream = mediaRequest(q)
	out, err := a.accessCall(q.Context(), command)
	if err != nil {
		diagnosticReason(q, "订阅：到期、额度、并发或登录授权检查失败")
		apiError(w, 403, err.Error())
		return q, nil, done, true
	}
	if d, ok := q.Context().Value(detailKey{}).(*diagnosticDetail); ok {
		d.DeviceIdentity = out.DeviceIdentity
	}
	command.Lease = out.Lease
	requestContext := q.Context()
	stop := make(chan struct{})
	done = func() {
		close(stop)
		ctx, end := context.WithTimeout(context.Background(), 3*time.Second)
		defer end()
		finish := command
		finish.Action = "finish"
		mw.mu.Lock()
		finish.Status = mw.responseStatus
		if finish.Status == 0 {
			finish.Status = 200
		}
		mw.mu.Unlock()
		a.accessCall(ctx, finish)
	}
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-requestContext.Done():
				return
			case <-ticker.C:
				check := command
				check.Action = "check"
				if _, e := a.accessCall(requestContext, check); e != nil {
					cancel()
					mw.closeConnection()
					return
				}
			}
		}
	}()
	mw.accessCharge = func(n int) error {
		charge := command
		charge.Action = "charge"
		charge.Bytes = int64(n)
		result, e := a.accessCall(requestContext, charge)
		if e != nil {
			return e
		}
		if result.WaitNS > 0 {
			timer := time.NewTimer(time.Duration(result.WaitNS))
			defer timer.Stop()
			select {
			case <-requestContext.Done():
				return requestContext.Err()
			case <-timer.C:
			}
			charge.Action = "check"
			if _, e = a.accessCall(requestContext, charge); e != nil {
				return e
			}
		}
		return nil
	}
	gateway := rr.gateway
	if command.Stream {
		q = q.WithContext(proxy.WithForcedMedia(requestContext))
	}
	replaceAccessAuth(q, out.SourceToken)
	if err := applyDeviceIdentity(q, out.DeviceIdentity, out.SourceToken); err != nil {
		apiError(w, 400, err.Error())
		return q, nil, done, true
	}
	if strings.HasPrefix(path, "/__access_media/") {
		resolve := command
		resolve.Action = "resolve"
		resolve.Target = strings.TrimPrefix(path, "/__access_media/")
		targetResult, e := a.accessCall(requestContext, resolve)
		if e != nil {
			apiError(w, 403, e.Error())
			return q, nil, done, true
		}
		target, e := url.Parse(targetResult.Target)
		if e != nil {
			apiError(w, 502, "invalid media target")
			return q, nil, done, true
		}
		var upstream Upstream
		for _, u := range a.state.Load().snapshot.Upstreams {
			if u.ID == rr.route.UpstreamID {
				upstream = u
			}
		}
		source, _ := url.Parse(upstream.BaseURL)
		same := source != nil && strings.EqualFold(source.Host, target.Host) && source.Scheme == target.Scheme
		if !same {
			replaceAccessAuth(q, "")
		}
		policy := proxy.NetworkPolicy{}
		for _, s := range upstream.AllowedCIDRs {
			p, e := netip.ParsePrefix(s)
			if e == nil {
				policy.Allowed = append(policy.Allowed, p)
			}
		}
		q.URL.Path = target.Path
		q.URL.RawPath = target.RawPath
		q.URL.RawQuery = target.RawQuery
		if same {
			// Wrapped targets can contain their own query DeviceId; normalize again.
			if e := applyDeviceIdentity(q, out.DeviceIdentity, out.SourceToken); e != nil {
				apiError(w, 400, e.Error())
				return q, nil, done, true
			}
		} else {
			for _, header := range []string{"X-Emby-Device-Id", "X-Emby-Device-Name", "X-Emby-Client", "X-Emby-Client-Version"} {
				q.Header.Del(header)
			}
		}
		base := *target
		base.Path = ""
		base.RawPath = ""
		base.RawQuery = ""
		gateway = proxy.New(proxy.Config{Upstream: &base, Network: policy, TrustedAuth: true, AutoMediaRedirect: true, Mode: "proxy", MaxConcurrent: rr.route.MaxConcurrent})
		previous := done
		done = func() { gateway.CloseIdleConnections(); previous() }
		q = q.WithContext(proxy.WithForcedMedia(requestContext))
	}
	wrap := func(target string) (string, error) {
		in := command
		in.Action = "wrap"
		in.Target = target
		result, e := a.accessCall(requestContext, in)
		if e != nil {
			return "", e
		}
		return prefix + "/__access_media/" + result.Target + "?api_key=" + url.QueryEscape(command.Token), nil
	}
	q = q.WithContext(proxy.WithAccessProtection(q.Context(), out.SourceToken, command.Token, wrap))
	return q, gateway, done, false
}

// Called by the subscription watchdog, including idle WebSocket connections.
func (w *meterWriter) closeConnection() {
	w.mu.Lock()
	c := w.connection
	w.mu.Unlock()
	if c != nil {
		c.Close()
	}
}
