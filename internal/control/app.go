package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"embyrelay/internal/database"
	"embyrelay/internal/proxy"
	"embyrelay/internal/version"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type App struct {
	AccessRPC        func(context.Context, AccessCommand) (AccessResult, error)
	accessMu         sync.Mutex
	AcquireQuota     func(context.Context, Route, int64) error
	Store            *Store
	SecureCookies    bool
	AgentMode        bool
	master           []byte
	signing          ed25519.PrivateKey
	dummyHash        string
	limiter          loginLimiter
	configMu         sync.Mutex
	state            atomic.Pointer[runtimeState]
	nodes            atomic.Pointer[[]Node]
	active           atomic.Int64
	requests         atomic.Int64
	ui               http.Handler
	runtimeMu        sync.Mutex
	usage            map[string]*routeRuntime
	diagnosticMu     sync.Mutex
	diagnosticQueue  chan Diagnostic
	diagnosticDone   chan struct{}
	diagnosticSample atomic.Uint64
	hijackedMu       sync.Mutex
	hijacked         map[net.Conn]struct{}
	mediaMu          sync.Mutex
	mediaGateways    map[string]*cachedMediaGateway
}

func (a *App) trackHijacked(c net.Conn) {
	a.hijackedMu.Lock()
	if a.hijacked == nil {
		a.hijacked = map[net.Conn]struct{}{}
	}
	a.hijacked[c] = struct{}{}
	a.hijackedMu.Unlock()
}
func (a *App) untrackHijacked(c net.Conn) {
	a.hijackedMu.Lock()
	delete(a.hijacked, c)
	a.hijackedMu.Unlock()
}
func (a *App) CloseHijacked() {
	a.hijackedMu.Lock()
	conns := make([]net.Conn, 0, len(a.hijacked))
	for c := range a.hijacked {
		conns = append(conns, c)
	}
	a.hijackedMu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

type routeRuntime struct {
	active atomic.Int64
	rateMu sync.Mutex
	next   time.Time
}
type runtimeRoute struct {
	route   Route
	gateway *proxy.Gateway
	keys    []Key
	usage   *routeRuntime
}
type cachedMediaGateway struct {
	gateway *proxy.Gateway
	used    time.Time
}

func (a *App) mediaGateway(key string, cfg proxy.Config) *proxy.Gateway {
	a.mediaMu.Lock()
	defer a.mediaMu.Unlock()
	if a.mediaGateways == nil {
		a.mediaGateways = map[string]*cachedMediaGateway{}
	}
	if cached := a.mediaGateways[key]; cached != nil {
		cached.used = time.Now()
		return cached.gateway
	}
	if len(a.mediaGateways) >= 128 {
		oldest := ""
		for id, cached := range a.mediaGateways {
			if oldest == "" || cached.used.Before(a.mediaGateways[oldest].used) {
				oldest = id
			}
		}
		a.mediaGateways[oldest].gateway.CloseIdleConnections()
		delete(a.mediaGateways, oldest)
	}
	gateway := proxy.New(cfg)
	a.mediaGateways[key] = &cachedMediaGateway{gateway: gateway, used: time.Now()}
	return gateway
}
func (a *App) trimMediaGateways() {
	a.mediaMu.Lock()
	defer a.mediaMu.Unlock()
	for key, cached := range a.mediaGateways {
		if time.Since(cached.used) > 5*time.Minute {
			cached.gateway.CloseIdleConnections()
			delete(a.mediaGateways, key)
		}
	}
}

type runtimeState struct {
	snapshot Snapshot
	routes   map[string]*runtimeRoute
}

func New(store *Store, master []byte, secure bool, assets fs.FS) (*App, error) {
	if len(master) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes")
	}
	dummy, _ := password(ID())
	a := &App{Store: store, SecureCookies: secure, master: master, dummyHash: dummy, usage: map[string]*routeRuntime{}}
	if assets != nil {
		a.ui = http.FileServer(http.FS(assets))
	}
	saved := store.Setting("signing_key")
	if saved == "" {
		_, priv, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return nil, e
		}
		v, e := a.seal(base64.RawStdEncoding.EncodeToString(priv))
		if e != nil {
			return nil, e
		}
		if e = store.SetSetting("signing_key", v); e != nil {
			return nil, e
		}
		a.signing = priv
	} else {
		v, e := a.unseal(saved)
		if e != nil {
			return nil, fmt.Errorf("master key cannot decrypt store")
		}
		b, e := base64.RawStdEncoding.DecodeString(v)
		if e != nil || len(b) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("invalid signing key")
		}
		a.signing = b
	}
	if e := a.Reload(); e != nil {
		return nil, e
	}
	return a, nil
}
func (a *App) Reload() error {
	snapshot, e := a.Store.Snapshot()
	if e != nil {
		return e
	}
	if e = a.Apply(snapshot); e != nil {
		return e
	}
	nodes, e := list[Node](a.Store, "node")
	if e == nil {
		a.nodes.Store(&nodes)
	}
	return e
}
func (a *App) Apply(s Snapshot) error {
	next := &runtimeState{snapshot: s, routes: map[string]*runtimeRoute{}}
	up := map[string]Upstream{}
	for _, u := range s.Upstreams {
		if e := validateUpstream(u); e != nil {
			return e
		}
		up[u.ID] = u
	}
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	for _, r := range s.Routes {
		u, ok := up[r.UpstreamID]
		if !ok || !u.Enabled || !r.Enabled {
			continue
		}
		target, e := proxy.ValidateURL(u.BaseURL)
		if e != nil {
			return e
		}
		p := proxy.NetworkPolicy{}
		for _, raw := range u.AllowedCIDRs {
			prefix, e := netip.ParsePrefix(raw)
			if e != nil {
				return e
			}
			p.Allowed = append(p.Allowed, prefix)
		}
		runtime := a.usage[r.ID]
		if runtime == nil {
			runtime = &routeRuntime{}
			a.usage[r.ID] = runtime
		}
		cfg := proxy.Config{AutoMediaRedirect: true, Upstream: target, AccessKey: ID(), Network: p, MaxConcurrent: r.MaxConcurrent, MaxResumes: 32, Mode: "proxy", RedirectHosts: u.RedirectHosts, TrustedAuth: true}
		rr := &runtimeRoute{route: r, gateway: proxy.New(cfg), usage: runtime}
		for _, k := range s.Keys {
			if k.RouteID == r.ID {
				rr.keys = append(rr.keys, k)
			}
		}
		next.routes[r.Alias] = rr
	}
	old := a.state.Swap(next)
	a.mediaMu.Lock()
	for key, cached := range a.mediaGateways {
		cached.gateway.CloseIdleConnections()
		delete(a.mediaGateways, key)
	}
	a.mediaMu.Unlock()
	if old != nil {
		for _, r := range old.routes {
			r.gateway.CloseIdleConnections()
		}
	}
	return nil
}
func (a *App) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if strings.HasPrefix(r.URL.Path, "/r/") {
		a.play(w, r)
		return
	}
	if r.URL.Path == "/healthz" || r.URL.Path == "/api/v1/health" {
		if a.Store.Check() != nil {
			apiError(w, 503, "database unavailable")
			return
		}
		writeJSON(w, 200, map[string]any{"status": "ok", "version": version.Version, "agent": a.AgentMode, "database": a.Store.DB.Driver})
		return
	}
	if a.AgentMode {
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/portal/") {
		a.portal(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/agent/") {
		a.agentAPI(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/") || r.URL.Path == "/metrics" {
		if !sameOrigin(r) {
			apiError(w, 403, "cross-origin request rejected")
			return
		}
		if r.URL.Path == "/api/v1/auth/login" && r.Method == "POST" {
			a.login(w, r)
			return
		}
		user, e := a.current(r)
		if e != nil {
			apiError(w, 401, "authentication required")
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("X-Requested-With") != "EmbyRelay" {
			apiError(w, 403, "CSRF header required")
			return
		}
		if user.Role != "admin" && r.Method != "GET" && r.URL.Path != "/api/v1/auth/logout" && !(r.Method == "POST" && r.URL.Path == "/api/v1/redirect-requests") {
			apiError(w, 403, "administrator role required")
			return
		}
		a.admin(w, r, user)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'")
	if a.ui != nil {
		a.ui.ServeHTTP(w, r)
		return
	}
	http.NotFound(w, r)
}

var aliasPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func validateUpstream(u Upstream) error {
	if err := validateDevicePolicy(u.DevicePolicy); err != nil {
		return err
	}
	if !safeName(u.Name) {
		return fmt.Errorf("name required (max 80 characters)")
	}
	if _, e := proxy.ValidateURL(u.BaseURL); e != nil {
		return e
	}
	if u.Mode != "proxy" && u.Mode != "redirect" && u.Mode != "auto" {
		return fmt.Errorf("mode must be proxy, redirect or auto")
	}
	for _, v := range u.AllowedCIDRs {
		if _, e := netip.ParsePrefix(v); e != nil {
			return fmt.Errorf("invalid allowed CIDR")
		}
	}
	for _, h := range u.RedirectHosts {
		v, e := url.Parse("https://" + h)
		if e != nil || v.Host != h || v.Path != "" || v.User != nil || strings.Contains(h, "*") {
			return fmt.Errorf("redirect allowlist requires exact hosts including optional port")
		}
	}
	return nil
}
func (a *App) admin(w http.ResponseWriter, r *http.Request, user User) {
	p := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	parts := strings.Split(strings.Trim(p, "/"), "/")
	resource := parts[0]
	id := ""
	if len(parts) > 1 {
		id = parts[1]
	}
	switch {
	case p == "settings/security":
		a.securitySettings(w, r, user)
	case p == "auth/me" && r.Method == "GET":
		writeJSON(w, 200, map[string]any{"id": user.ID, "username": user.Username, "role": user.Role, "totp_enabled": user.TOTP != ""})
	case p == "auth/logout" && r.Method == "POST":
		if c, e := r.Cookie("relay_session"); e == nil {
			a.Store.DB.Exec("DELETE FROM login_sessions WHERE hash=?", Hash(c.Value))
		}
		http.SetCookie(w, &http.Cookie{Name: "relay_session", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.SecureCookies, SameSite: http.SameSiteStrictMode})
		writeJSON(w, 200, map[string]bool{"ok": true})
	case p == "auth/totp" && r.Method == "POST":
		a.totpAPI(w, r, user)
	case p == "auth/password" && r.Method == "POST":
		a.changeOwnPassword(w, r, user)
	case resource == "upstreams" && len(parts) == 3 && parts[2] == "credentials":
		a.revealCredentials(w, r, user, id)
	case resource == "upstreams" && len(parts) == 3 && parts[2] == "health" && r.Method == "GET":
		a.probe(w, r, id)
	case resource == "device-records" && r.Method == "GET":
		if user.Role != "admin" {
			apiError(w, 403, "administrator role required")
			return
		}
		a.deviceRecords(w, r, "")
	case resource == "upstreams" && len(parts) == 3 && parts[2] == "device-policy":
		a.devicePolicyAPI(w, r, user, id, "")
	case resource == "access-accounts" && len(parts) == 3 && parts[2] == "device-policy":
		a.devicePolicyAPI(w, r, user, r.URL.Query().Get("upstream_id"), id)
	case resource == "access-plans" || resource == "access-accounts":
		a.accessAdmin(w, r, user, resource, id, parts)
	case p == "user-sources":
		a.userSources(w, r, user)
	case resource == "upstreams":
		a.upstreams(w, r, user, id)
	case resource == "routes":
		a.routes(w, r, user, id, parts)
	case resource == "access-keys" && r.Method == "DELETE":
		a.mutate(w, func() error { return a.Store.remove("key", id, user.ID) })
	case resource == "users":
		a.users(w, r, user, id)
	case resource == "relays":
		a.relays(w, r, user, id)
	case p == "sessions" && r.Method == "GET":
		v, e := a.Store.Sessions(intParam(r, "limit", 100))
		if e != nil {
			apiError(w, 500, "storage unavailable")
			return
		}
		writeJSON(w, 200, v)
	case p == "traffic" && r.Method == "GET":
		v, e := a.Store.Traffic()
		if e != nil {
			apiError(w, 500, "storage unavailable")
			return
		}
		writeJSON(w, 200, v)
	case resource == "redirect-requests":
		apiError(w, 410, "跳转审批已移除，媒体自动中转")
	case p == "diagnostics" && r.Method == "GET":
		node := r.URL.Query().Get("node_id")
		query := "SELECT id,status,reason,created_at,method,path,source,details,node_id,node_name FROM request_diagnostics"
		if node == "local" {
			a.queryRows(w, query+" WHERE node_id='' ORDER BY created_at DESC,id DESC LIMIT ?", intParam(r, "limit", 100))
		} else if node != "" {
			a.queryRows(w, query+" WHERE node_id=? ORDER BY created_at DESC,id DESC LIMIT ?", node, intParam(r, "limit", 100))
		} else {
			a.queryRows(w, query+" ORDER BY created_at DESC,id DESC LIMIT ?", intParam(r, "limit", 100))
		}
	case p == "audit" && r.Method == "GET":
		a.queryRows(w, "SELECT id,actor_id,action,target,created_at FROM audit_logs ORDER BY id DESC LIMIT ?", intParam(r, "limit", 100))
	case p == "overview" && r.Method == "GET":
		s := a.state.Load()
		traffic, _ := a.Store.Traffic()
		writeJSON(w, 200, map[string]any{"version": s.snapshot.Version, "active_requests": a.active.Load(), "requests": a.requests.Load(), "upstreams": len(s.snapshot.Upstreams), "routes": len(s.snapshot.Routes), "traffic": traffic})
	case r.URL.Path == "/metrics" && r.Method == "GET":
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "# TYPE embyrelay_active_requests gauge\nembyrelay_active_requests %d\n# TYPE embyrelay_requests_total counter\nembyrelay_requests_total %d\n", a.active.Load(), a.requests.Load())
		traffic, _ := a.Store.Traffic()
		for _, t := range traffic {
			fmt.Fprintf(w, "embyrelay_egress_bytes{route=%q,date=%q} %d\nembyrelay_ingress_bytes{route=%q,date=%q} %d\n", t.RouteID, t.Date, t.Egress, t.RouteID, t.Date, t.Ingress)
		}
	default:
		apiError(w, 404, "endpoint not found")
	}
}
func (a *App) mutate(w http.ResponseWriter, fn func() error) {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	if e := fn(); e != nil {
		apiError(w, 400, e.Error())
		return
	}
	if e := a.Reload(); e != nil {
		apiError(w, 500, "configuration reload failed")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) upstreams(w http.ResponseWriter, r *http.Request, u User, id string) {
	if r.Method == "GET" {
		v, e := list[Upstream](a.Store, "upstream")
		if e != nil {
			apiError(w, 500, "storage unavailable")
			return
		}
		for i := range v {
			v[i].HasCredentials = v[i].EncryptedCredentials != ""
			v[i].EncryptedCredentials = ""
			v[i].Mode = "proxy"
		}
		writeJSON(w, 200, v)
		return
	}
	if r.Method == "DELETE" {
		a.mutate(w, func() error {
			rs, e := list[Route](a.Store, "route")
			if e != nil {
				return e
			}
			for _, x := range rs {
				if x.UpstreamID == id {
					return fmt.Errorf("remove dependent routes first")
				}
			}
			return a.Store.remove("upstream", id, u.ID)
		})
		return
	}
	if r.Method != "POST" && r.Method != "PATCH" {
		apiError(w, 405, "method not allowed")
		return
	}
	var v Upstream
	if r.Method == "PATCH" {
		var e error
		v, e = get[Upstream](a.Store, "upstream", id)
		if e != nil {
			apiError(w, 404, "upstream not found")
			return
		}
	} else {
		v.Enabled = true
		v.Mode = "proxy"
	}
	savedOwner := v.OwnerAccountID
	savedCredentials := v.EncryptedCredentials
	input := struct {
		*Upstream
		Username *string `json:"emby_username"`
		Password *string `json:"emby_password"`
		Clear    bool    `json:"clear_credentials"`
	}{Upstream: &v}
	if !decode(w, r, &input) {
		return
	}
	v.OwnerAccountID = savedOwner
	v.EncryptedCredentials = savedCredentials
	v.HasCredentials = false
	v.Mode = "proxy"
	v.RedirectHosts = nil
	if err := a.updateCredentials(&v, input.Username, input.Password, input.Clear); err != nil {
		apiError(w, 400, "账号信息保存失败")
		return
	}
	normalized, err := normalizeUpstreamURL(v.BaseURL)
	if err != nil {
		apiError(w, 400, err.Error())
		return
	}
	v.BaseURL = normalized
	if e := validateUpstream(v); e != nil {
		apiError(w, 400, e.Error())
		return
	}
	if r.Method == "POST" {
		id = ID()
	}
	v.ID = id
	a.mutate(w, func() error { return a.Store.put("upstream", id, v, u.ID) })
}
func (a *App) routes(w http.ResponseWriter, r *http.Request, u User, id string, parts []string) {
	if len(parts) == 3 && parts[2] == "keys" {
		if u.Role != "admin" {
			apiError(w, 403, "administrator role required")
			return
		}
		route, e := get[Route](a.Store, "route", id)
		if e != nil {
			apiError(w, 404, "route not found")
			return
		}
		if r.Method == "GET" {
			all, e := list[Key](a.Store, "key")
			if e != nil {
				apiError(w, 500, "storage unavailable")
				return
			}
			out := []Key{}
			for _, k := range all {
				if k.RouteID == id {
					k.Hash = ""
					if k.EncryptedSecret != "" {
						var err error
						k.AccessKey, err = a.unseal(k.EncryptedSecret)
						if err != nil {
							apiError(w, 500, "access key decryption failed")
							return
						}
					}
					k.EncryptedSecret = ""
					out = append(out, k)
				}
			}
			writeJSON(w, 200, out)
			return
		}
		if r.Method == "POST" {
			var in struct {
				ExpiresAt int64 `json:"expires_at"`
			}
			if !decode(w, r, &in) {
				return
			}
			if in.ExpiresAt != 0 && in.ExpiresAt <= time.Now().Unix() {
				apiError(w, 400, "expiry must be in the future")
				return
			}
			secret := ID()
			key := Key{ID: ID(), RouteID: id, AccountID: route.OwnerAccountID, Hash: Hash(secret), Enabled: true, ExpiresAt: in.ExpiresAt}
			encrypted, err := a.seal(secret)
			if err != nil {
				apiError(w, 500, "key encryption failed")
				return
			}
			key.EncryptedSecret = encrypted
			a.configMu.Lock()
			defer a.configMu.Unlock()
			if e := a.Store.put("key", key.ID, key, u.ID); e != nil {
				apiError(w, 500, "key storage failed")
				return
			}
			if a.Reload() != nil {
				apiError(w, 500, "reload failed")
				return
			}
			writeJSON(w, 201, map[string]string{"id": key.ID, "access_key": secret})
			return
		}
	}
	if r.Method == "GET" {
		v, e := list[Route](a.Store, "route")
		if e != nil {
			apiError(w, 500, "storage unavailable")
			return
		}
		writeJSON(w, 200, v)
		return
	}
	if r.Method == "DELETE" {
		a.mutate(w, func() error { return a.Store.remove("route", id, u.ID) })
		return
	}
	if r.Method != "POST" && r.Method != "PATCH" {
		apiError(w, 405, "method not allowed")
		return
	}
	var v Route
	if r.Method == "PATCH" {
		var e error
		v, e = get[Route](a.Store, "route", id)
		if e != nil {
			apiError(w, 404, "route not found")
			return
		}
	} else {
		v.Enabled = true
		v.MaxConcurrent = 100
	}
	if !decode(w, r, &v) {
		return
	}
	if !aliasPattern.MatchString(v.Alias) || v.RateBPS < 0 || v.DailyQuota < 0 || v.MaxConcurrent < 1 || v.MaxConcurrent > 10000 {
		apiError(w, 400, "invalid alias, limits or concurrency")
		return
	}
	source, e := get[Upstream](a.Store, "upstream", v.UpstreamID)
	if e != nil {
		apiError(w, 400, "upstream not found")
		return
	}
	v.OwnerAccountID = source.OwnerAccountID
	if v.RelayID != "" {
		if v.RelayID != "auto" {
			if _, e := get[Node](a.Store, "node", v.RelayID); e != nil {
				apiError(w, 400, "relay not found")
				return
			}
		}

	}
	if r.Method == "POST" {
		id = ID()
	}
	v.ID = id
	a.mutate(w, func() error {
		all, e := list[Route](a.Store, "route")
		if e != nil {
			return e
		}
		for _, x := range all {
			if x.Alias == v.Alias && x.ID != id {
				return fmt.Errorf("alias already exists")
			}
		}
		return a.Store.put("route", id, v, u.ID)
	})
}
func (a *App) users(w http.ResponseWriter, r *http.Request, u User, id string) {
	if u.Role != "admin" {
		apiError(w, 403, "administrator role required")
		return
	}
	if r.Method == "GET" {
		a.queryRows(w, "SELECT id,username,role,CASE WHEN totp!='' THEN 1 ELSE 0 END AS totp_enabled FROM users ORDER BY username")
		return
	}
	a.configMu.Lock()
	defer a.configMu.Unlock()
	var currentRole string
	if a.Store.DB.QueryRow("SELECT role FROM users WHERE id=?", u.ID).Scan(&currentRole) != nil || currentRole != "admin" {
		apiError(w, 403, "administrator role required")
		return
	}
	if r.Method == "DELETE" {
		if id == u.ID {
			apiError(w, 400, "cannot delete your own account")
			return
		}
		if e := a.changeUser(u.ID, id, "delete_user", func(tx *database.Tx) error {
			result, e := tx.Exec("DELETE FROM users WHERE id=?", id)
			if e != nil {
				return e
			}
			n, _ := result.RowsAffected()
			if n != 1 {
				return fmt.Errorf("user not found")
			}
			return nil
		}); e != nil {
			apiError(w, 400, "user not found or storage unavailable")
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	if r.Method != "POST" && r.Method != "PATCH" {
		apiError(w, 405, "method not allowed")
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !safeName(in.Username) || (in.Role != "admin" && in.Role != "operator") {
		apiError(w, 400, "invalid username or role")
		return
	}
	if id == u.ID && in.Role != "admin" {
		apiError(w, 400, "cannot demote your own account")
		return
	}
	h, e := password(in.Password)
	if e != nil {
		apiError(w, 400, e.Error())
		return
	}
	if r.Method == "POST" {
		id = ID()
	}
	e = a.changeUser(u.ID, id, "save_user", func(tx *database.Tx) error {
		if r.Method == "POST" {
			_, err := tx.Exec("INSERT INTO users(id,username,password_hash,role) VALUES(?,?,?,?)", id, in.Username, h, in.Role)
			return err
		}
		result, err := tx.Exec("UPDATE users SET username=?,password_hash=?,role=? WHERE id=?", in.Username, h, in.Role, id)
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return fmt.Errorf("user not found")
		}
		_, err = tx.Exec("DELETE FROM login_sessions WHERE user_id=?", id)
		return err
	})
	if e != nil {
		apiError(w, 400, "user not found, username conflict or storage failure")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) totpAPI(w http.ResponseWriter, r *http.Request, u User) {
	var in struct {
		Action   string `json:"action"`
		Secret   string `json:"secret"`
		Code     string `json:"code"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !verifyPassword(u.PasswordHash, in.Password) {
		apiError(w, 401, "password required")
		return
	}
	if in.Action == "setup" {
		if u.TOTP != "" {
			apiError(w, 409, "disable existing TOTP first")
			return
		}
		b := make([]byte, 20)
		if _, err := rand.Read(b); err != nil {
			apiError(w, 500, "secret generation failed")
			return
		}
		secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
		writeJSON(w, 200, map[string]string{"secret": secret, "uri": "otpauth://totp/EmbyRelay:" + url.PathEscape(u.Username) + "?secret=" + secret + "&issuer=EmbyRelay&algorithm=SHA1&digits=6&period=30"})
		return
	}
	var encrypted string
	if in.Action == "enable" {
		if u.TOTP != "" || len(in.Secret) != 32 || !totp(in.Secret, in.Code, time.Now()) {
			apiError(w, 400, "invalid secret or code")
			return
		}
		var err error
		encrypted, err = a.seal(in.Secret)
		if err != nil {
			apiError(w, 500, "secret encryption failed")
			return
		}
	} else if in.Action == "disable" {
		if !a.checkTOTP(u.TOTP, in.Code) {
			apiError(w, 400, "invalid code")
			return
		}
	} else {
		apiError(w, 400, "invalid action")
		return
	}
	if _, e := a.Store.DB.Exec("UPDATE users SET totp=? WHERE id=?", encrypted, u.ID); e != nil {
		apiError(w, 500, "storage failed")
		return
	}
	if in.Action == "disable" {
		a.Store.DB.Exec("DELETE FROM totp_replay WHERE user_id=?", u.ID)
	}
	a.Store.DB.Exec("DELETE FROM login_sessions WHERE user_id=?", u.ID)
	a.Store.Audit(u.ID, "totp_"+in.Action, u.ID)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) queryRows(w http.ResponseWriter, q string, args ...any) {
	rows, e := a.Store.DB.Query(q, args...)
	if e != nil {
		apiError(w, 500, "storage unavailable")
		return
	}
	defer rows.Close()
	columns, _ := rows.Columns()
	out := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if rows.Scan(ptrs...) != nil {
			apiError(w, 500, "storage unavailable")
			return
		}
		m := map[string]any{}
		for i, k := range columns {
			m[k] = values[i]
		}
		out = append(out, m)
	}
	writeJSON(w, 200, out)
}
func (a *App) Maintenance(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.Store.Cleanup()
			a.trimMediaGateways()
		}
	}
}
func Encode(v any) []byte { b, _ := json.Marshal(v); return b }

func (a *App) changeUser(actor, id, action string, fn func(*database.Tx) error) error {
	tx, e := a.Store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = fn(tx); e != nil {
		return e
	}
	if _, e = tx.Exec("INSERT INTO audit_logs(actor_id,action,target,created_at) VALUES(?,?,?,?)", actor, action, id, time.Now().Unix()); e != nil {
		return e
	}
	return tx.Commit()
}
