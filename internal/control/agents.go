package control

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Enrollment struct {
	Token     string `json:"token"`
	Name      string `json:"name"`
	Endpoint  string `json:"endpoint"`
	PublicKey string `json:"public_key"`
}
type EnrollmentResult struct {
	ID        string `json:"id"`
	ServerKey string `json:"server_key"`
}
type SignedSnapshot struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}
type Heartbeat struct {
	DeviceIdentitySupport bool      `json:"device_identity_support"`
	SubscriptionSupport   bool      `json:"subscription_support"`
	Sessions              []Session `json:"sessions"`
	Version               int64     `json:"version"`
	Traffic               []Traffic `json:"traffic"`
	Active                int64     `json:"active"`
}

func SignMessage(method, path, timestamp, nonce string, body []byte) []byte {
	return []byte(method + "\n" + path + "\n" + timestamp + "\n" + nonce + "\n" + Hash(string(body)))
}
func SignRequest(r *http.Request, body []byte, id string, key ed25519.PrivateKey) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := ID()
	r.Header.Set("X-Relay-Node", id)
	r.Header.Set("X-Relay-Timestamp", ts)
	r.Header.Set("X-Relay-Nonce", nonce)
	r.Header.Set("X-Relay-Signature", base64.RawStdEncoding.EncodeToString(ed25519.Sign(key, SignMessage(r.Method, r.URL.RequestURI(), ts, nonce, body))))
}
func VerifySnapshot(s SignedSnapshot, key ed25519.PublicKey) (Snapshot, error) {
	var out Snapshot
	b, e := base64.RawStdEncoding.DecodeString(s.Payload)
	if e != nil {
		return out, e
	}
	sig, e := base64.RawStdEncoding.DecodeString(s.Signature)
	if e != nil || !ed25519.Verify(key, b, sig) {
		return out, fmt.Errorf("invalid configuration signature")
	}
	e = json.Unmarshal(b, &out)
	return out, e
}
func (a *App) relays(w http.ResponseWriter, r *http.Request, u User, id string) {
	if r.Method == "GET" {
		nodes, e := list[Node](a.Store, "node")
		if e != nil {
			apiError(w, 500, "storage unavailable")
			return
		}
		writeJSON(w, 200, nodes)
		return
	}
	if id == "enroll" && r.Method == "POST" {
		token := ID()
		_, e := a.Store.DB.Exec("INSERT INTO enroll_tokens VALUES(?,?)", Hash(token), time.Now().Add(10*time.Minute).Unix())
		if e != nil {
			apiError(w, 500, "storage unavailable")
			return
		}
		a.Store.Audit(u.ID, "create_enrollment_token", "")
		writeJSON(w, 201, map[string]any{"token": token, "expires_in": 600, "server_key": base64.RawStdEncoding.EncodeToString(a.signing.Public().(ed25519.PublicKey))})
		return
	}
	if r.Method == "DELETE" {
		a.mutate(w, func() error {
			rs, e := list[Route](a.Store, "route")
			if e != nil {
				return e
			}
			for _, x := range rs {
				if x.RelayID == id {
					return fmt.Errorf("unassign routes first")
				}
			}
			return a.Store.remove("node", id, u.ID)
		})
		return
	}
	if r.Method == "PATCH" {
		var in struct {
			Enabled bool `json:"enabled"`
		}
		if !decode(w, r, &in) {
			return
		}
		a.mutate(w, func() error {
			v, err := get[Node](a.Store, "node", id)
			if err != nil {
				return fmt.Errorf("node not found")
			}
			v.Enabled = in.Enabled
			return a.Store.put("node", id, v, u.ID)
		})
		return
	}
	apiError(w, 405, "method not allowed")
}
func verifySigned(r *http.Request, body []byte, pub ed25519.PublicKey) bool {
	ts, e := strconv.ParseInt(r.Header.Get("X-Relay-Timestamp"), 10, 64)
	delta := time.Now().Unix() - ts
	if e != nil || delta > 120 || delta < -120 {
		return false
	}
	nonce := r.Header.Get("X-Relay-Nonce")
	if len(nonce) != 48 {
		return false
	}
	sig, e := base64.RawStdEncoding.DecodeString(r.Header.Get("X-Relay-Signature"))
	return e == nil && len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, SignMessage(r.Method, r.URL.RequestURI(), r.Header.Get("X-Relay-Timestamp"), nonce, body), sig)
}
func (a *App) agentAPI(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	body, e := io.ReadAll(r.Body)
	if e != nil {
		apiError(w, 400, "body too large")
		return
	}
	if r.URL.Path == "/api/v1/agent/enroll" && r.Method == "POST" {
		ip := clientIP(r)
		if !a.limiter.allow("enroll", ip, "") {
			apiError(w, 429, "too many enrollment attempts")
			return
		}
		var in Enrollment
		if json.Unmarshal(body, &in) != nil || !safeName(in.Name) {
			a.limiter.fail("enroll", ip, "")
			apiError(w, 400, "invalid enrollment")
			return
		}
		pub, e := base64.RawStdEncoding.DecodeString(in.PublicKey)
		if e != nil || !verifySigned(r, body, pub) {
			a.limiter.fail("enroll", ip, "")
			apiError(w, 401, "invalid enrollment signature")
			return
		}
		u, e := url.Parse(in.Endpoint)
		if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && !(u.Scheme == "http" && !a.SecureCookies)) {
			apiError(w, 400, "relay requires an HTTPS origin (HTTP only in explicit insecure mode)")
			return
		}
		a.configMu.Lock()
		defer a.configMu.Unlock()
		node := Node{ID: ID(), Name: in.Name, Endpoint: strings.TrimRight(in.Endpoint, "/"), PublicKey: in.PublicKey, Enabled: true}
		tx, e := a.Store.DB.Begin()
		if e != nil {
			apiError(w, 500, "storage unavailable")
			return
		}
		defer tx.Rollback()
		result, e := tx.Exec("DELETE FROM enroll_tokens WHERE hash=? AND expires_at>?", Hash(in.Token), time.Now().Unix())
		if e != nil {
			apiError(w, 500, "storage unavailable")
			return
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			a.limiter.fail("enroll", ip, "")
			apiError(w, 401, "expired or consumed enrollment token")
			return
		}
		if _, e = tx.Exec("INSERT INTO objects VALUES('node',?,?,?)", node.ID, string(Encode(node)), time.Now().Unix()); e != nil {
			apiError(w, 500, "storage unavailable")
			return
		}
		tx.Exec("INSERT INTO audit_logs(actor_id,action,target,created_at) VALUES('agent','enroll',?,?)", node.ID, time.Now().Unix())
		if tx.Commit() != nil {
			apiError(w, 500, "enrollment failed")
			return
		}
		a.Reload()
		writeJSON(w, 201, EnrollmentResult{ID: node.ID, ServerKey: base64.RawStdEncoding.EncodeToString(a.signing.Public().(ed25519.PublicKey))})
		return
	}
	node, e := get[Node](a.Store, "node", r.Header.Get("X-Relay-Node"))
	if e != nil || !node.Enabled {
		apiError(w, 401, "unknown or disabled node")
		return
	}
	pub, e := base64.RawStdEncoding.DecodeString(node.PublicKey)
	if e != nil || !verifySigned(r, body, pub) {
		apiError(w, 401, "invalid agent signature")
		return
	}
	if _, e = a.Store.DB.Exec("INSERT INTO nonces VALUES(?,?,?)", node.ID, r.Header.Get("X-Relay-Nonce"), time.Now().Unix()); e != nil {
		apiError(w, 409, "replayed request")
		return
	}
	switch {
	case r.URL.Path == "/api/v1/agent/diagnostics" && r.Method == "POST":
		var batch DiagnosticBatch
		if json.Unmarshal(body, &batch) != nil {
			apiError(w, 400, "invalid diagnostic batch")
			return
		}
		ack, err := a.receiveDiagnostics(node, batch)
		if err != nil {
			apiError(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, ack)
	case r.URL.Path == "/api/v1/agent/access" && r.Method == "POST":
		if !node.SubscriptionSupport || !node.DeviceIdentitySupport {
			writeJSON(w, 200, AccessResult{Error: "中转节点需要同步升级设备身份支持", Code: 403})
			return
		}
		var in AccessCommand
		if json.Unmarshal(body, &in) != nil {
			apiError(w, 400, "invalid subscription request")
			return
		}
		result, err := a.accessCommand(r.Context(), node.ID, in)
		if err != nil {
			result = AccessResult{Error: err.Error(), Code: 403}
		}
		writeJSON(w, 200, result)
	case r.URL.Path == "/api/v1/agent/quota" && r.Method == "POST":
		var in QuotaRequest
		if json.Unmarshal(body, &in) != nil {
			apiError(w, 400, "invalid lease request")
			return
		}
		result, e := a.grantQuota(node, in)
		if e != nil {
			apiError(w, 429, e.Error())
			return
		}
		writeJSON(w, 200, result)
	case r.URL.Path == "/api/v1/agent/config" && r.Method == "GET":
		source := a.state.Load().snapshot
		s := Snapshot{AutoMediaRedirect: source.AutoMediaRedirect, Version: source.Version, GeneratedAt: time.Now().Unix(), Upstreams: []Upstream{}, Routes: []Route{}, Keys: []Key{}}
		ids := map[string]bool{}
		routes := map[string]bool{}
		for _, route := range source.Routes {
			if route.RelayID == node.ID || route.RelayID == "auto" {
				s.Routes = append(s.Routes, route)
				ids[route.UpstreamID] = true
				routes[route.ID] = true
			}
		}
		for _, up := range source.Upstreams {
			if ids[up.ID] {
				s.Upstreams = append(s.Upstreams, up)
			}
		}
		for _, key := range source.Keys {
			if routes[key.RouteID] && (key.AccountID == "" || (node.SubscriptionSupport && node.DeviceIdentitySupport)) {
				s.Keys = append(s.Keys, key)
			}
		}
		b := Encode(s)
		writeJSON(w, 200, SignedSnapshot{Payload: base64.RawStdEncoding.EncodeToString(b), Signature: base64.RawStdEncoding.EncodeToString(ed25519.Sign(a.signing, b))})
	case r.URL.Path == "/api/v1/agent/heartbeat" && r.Method == "POST":
		var in Heartbeat
		if json.Unmarshal(body, &in) != nil || len(in.Traffic) > 1000 || len(in.Sessions) > 100 || in.Active < 0 {
			apiError(w, 400, "invalid report")
			return
		}
		if e := a.report(node, in); e != nil {
			apiError(w, 400, e.Error())
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	default:
		apiError(w, 404, "endpoint not found")
	}
}
func (a *App) report(node Node, in Heartbeat) error {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	// Refresh under the same lock used by administrator updates. A heartbeat
	// must not restore an older Enabled value after the node was disabled.
	fresh, err := get[Node](a.Store, "node", node.ID)
	if err != nil || !fresh.Enabled {
		return fmt.Errorf("node disabled or unavailable")
	}
	node = fresh
	state := a.state.Load()
	allowed := map[string]bool{}
	for _, r := range state.snapshot.Routes {
		if r.RelayID == node.ID || r.RelayID == "auto" {
			allowed[r.ID] = true
		}
	}
	tx, e := a.Store.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	for _, t := range in.Traffic {
		if !allowed[t.RouteID] {
			continue
		}
		day, e := time.Parse("2006-01-02", t.Date)
		if e != nil || day.After(time.Now()) || time.Since(day) > 32*24*time.Hour || t.Ingress < 0 || t.Egress < 0 {
			return fmt.Errorf("invalid traffic report")
		}
		id := node.ID + ":" + t.Date + ":" + t.RouteID
		var previous Traffic
		var raw string
		e = tx.QueryRow("SELECT payload FROM objects WHERE kind='relay_traffic' AND id=?", id).Scan(&raw)
		if e == nil {
			json.Unmarshal([]byte(raw), &previous)
		}
		// A short downstream write refunds the bytes reserved before writing.
		// Heartbeats can observe the reservation and then its small correction.
		// Keep rejecting larger counter regressions caused by a restored Agent DB.
		if t.Ingress < previous.Ingress || previous.Egress-t.Egress > 1<<20 {
			return fmt.Errorf("traffic counters cannot decrease; preserve agent database")
		}
		if _, e = tx.Exec("INSERT INTO traffic_daily VALUES(?,?,?,?) ON CONFLICT(date,route_id) DO UPDATE SET ingress_bytes=traffic_daily.ingress_bytes+excluded.ingress_bytes,egress_bytes=traffic_daily.egress_bytes+excluded.egress_bytes", t.Date, t.RouteID, t.Ingress-previous.Ingress, t.Egress-previous.Egress); e != nil {
			return e
		}
		if _, e = tx.Exec("UPDATE quota_leases SET reported=MAX(reported,?) WHERE date=? AND route_id=? AND node_id=?", t.Egress, t.Date, t.RouteID, node.ID); e != nil {
			return e
		}
		if _, e = tx.Exec("INSERT INTO objects VALUES('relay_traffic',?,?,?) ON CONFLICT(kind,id) DO UPDATE SET payload=excluded.payload", id, string(Encode(t)), time.Now().Unix()); e != nil {
			return e
		}
	}
	for _, session := range in.Sessions {
		if !allowed[session.RouteID] {
			continue
		}
		if len(session.ID) > 64 || session.BytesSent < 0 || session.StartedAt <= 0 {
			return fmt.Errorf("invalid session report")
		}
		session.NodeID = node.ID
		if _, e = tx.Exec("INSERT INTO remote_sessions VALUES(?,?,?,?) ON CONFLICT(node_id,id) DO UPDATE SET payload=excluded.payload,updated_at=excluded.updated_at", node.ID, session.ID, string(Encode(session)), time.Now().Unix()); e != nil {
			return e
		}
	}
	node.LastSeen = time.Now().Unix()
	node.SubscriptionSupport = in.SubscriptionSupport
	node.DeviceIdentitySupport = in.DeviceIdentitySupport
	node.ConfigVersion = in.Version
	node.Active = in.Active
	if _, e = tx.Exec("UPDATE objects SET payload=? WHERE kind='node' AND id=?", string(Encode(node)), node.ID); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	nodes, e := list[Node](a.Store, "node")
	if e == nil {
		a.nodes.Store(&nodes)
	}
	return e
}
func (a *App) Active() int64 { return a.active.Load() }
