package relay

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"embyrelay/internal/control"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type ControlRequestError struct {
	Path   string
	Reason string
}

func (e *ControlRequestError) Error() string { return e.Reason }

type ControlHTTPError struct {
	Path   string
	Status int
}

func (e *ControlHTTPError) Error() string {
	return fmt.Sprintf("control plane %s returned %d", e.Path, e.Status)
}

type Identity struct {
	ID         string `json:"id"`
	PrivateKey string `json:"private_key"`
	ServerKey  string `json:"server_key"`
	ControlURL string `json:"control_url"`
}
type Agent struct {
	quotaLocks sync.Map
	App        *control.App
	Store      *control.Store
	Identity   Identity
	Client     *http.Client
	URL        string
	key        ed25519.PrivateKey
	pub        ed25519.PublicKey
	version    int64
	validUntil time.Time
	OfflineTTL time.Duration
}

func New(app *control.App, store *control.Store, dir, endpoint, controlURL, name, token, pinnedKey string, insecure bool) (*Agent, error) {
	if insecure && pinnedKey == "" {
		if _, e := os.Stat(filepath.Join(dir, "identity.json")); os.IsNotExist(e) {
			return nil, fmt.Errorf("insecure enrollment requires RELAY_SERVER_KEY pin")
		}
	}
	u, e := url.Parse(controlURL)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && !(insecure && u.Scheme == "http")) {
		return nil, fmt.Errorf("control URL requires HTTPS; HTTP needs RELAY_ALLOW_INSECURE=true")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 64
	a := &Agent{App: app, Store: store, URL: strings.TrimRight(controlURL, "/"), Client: &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}, OfflineTTL: 24 * time.Hour}
	app.AgentMode = true
	path := filepath.Join(dir, "identity.json")
	b, e := os.ReadFile(path)
	if e == nil {
		if json.Unmarshal(b, &a.Identity) != nil {
			return nil, fmt.Errorf("invalid agent identity")
		}
		if a.Identity.ControlURL != a.URL {
			return nil, fmt.Errorf("control URL differs from enrolled identity")
		}
	} else if os.IsNotExist(e) {
		_, priv, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return nil, e
		}
		a.key = priv
		enroll := control.Enrollment{Token: token, Name: name, Endpoint: endpoint, PublicKey: base64.RawStdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))}
		var result control.EnrollmentResult
		if e = a.request(context.Background(), "POST", "/api/v1/agent/enroll", enroll, &result); e != nil {
			return nil, e
		}
		if pinnedKey != "" && pinnedKey != result.ServerKey {
			return nil, fmt.Errorf("server signing key differs from pin")
		}
		if insecure && pinnedKey == "" {
			return nil, fmt.Errorf("insecure enrollment requires RELAY_SERVER_KEY pin")
		}
		a.Identity = Identity{ID: result.ID, PrivateKey: base64.RawStdEncoding.EncodeToString(priv), ServerKey: result.ServerKey, ControlURL: a.URL}
		if e = atomicWrite(path, control.Encode(a.Identity)); e != nil {
			return nil, e
		}
	} else {
		return nil, e
	}
	a.key, e = base64.RawStdEncoding.DecodeString(a.Identity.PrivateKey)
	if e != nil || len(a.key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid private key")
	}
	a.pub, e = base64.RawStdEncoding.DecodeString(a.Identity.ServerKey)
	if e != nil || len(a.pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid server key")
	}
	raw, cacheErr := app.LoadSecret("agent_snapshot")
	if cacheErr != nil {
		return nil, cacheErr
	}
	if raw != "" {
		var signed control.SignedSnapshot
		if json.Unmarshal([]byte(raw), &signed) == nil {
			snap, e := control.VerifySnapshot(signed, a.pub)
			if e == nil && time.Since(time.Unix(snap.GeneratedAt, 0)) < a.OfflineTTL {
				if e = app.Apply(snap); e != nil {
					return nil, e
				}
				a.version = snap.Version
				a.validUntil = time.Unix(snap.GeneratedAt, 0).Add(a.OfflineTTL)
			}
		}
	}
	store.AgentID = a.Identity.ID
	app.AcquireQuota = a.acquireQuota
	app.AccessRPC = func(ctx context.Context, in control.AccessCommand) (control.AccessResult, error) {
		var out control.AccessResult
		err := a.request(ctx, "POST", "/api/v1/agent/access", in, &out)
		return out, err
	}
	return a, nil
}
func atomicWrite(path string, b []byte) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".identity-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	f.Close()
	if e != nil {
		return e
	}
	return os.Rename(f.Name(), path)
}
func (a *Agent) request(ctx context.Context, method, path string, input, output any) error {
	var body []byte
	if input != nil {
		body = control.Encode(input)
	}
	r, e := http.NewRequestWithContext(ctx, method, a.URL+path, bytes.NewReader(body))
	if e != nil {
		return e
	}
	r.Header.Set("Content-Type", "application/json")
	control.SignRequest(r, body, a.Identity.ID, a.key)
	resp, e := a.Client.Do(r)
	if e != nil {
		return &ControlRequestError{Path: path, Reason: "control plane unreachable"}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &ControlHTTPError{Path: path, Status: resp.StatusCode}
	}
	if output == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(output); err != nil {
		return &ControlRequestError{Path: path, Reason: "invalid control plane response"}
	}
	return nil
}
func (a *Agent) Sync(ctx context.Context) error {
	var signed control.SignedSnapshot
	if e := a.request(ctx, "GET", "/api/v1/agent/config", nil, &signed); e != nil {
		return e
	}
	snap, e := control.VerifySnapshot(signed, a.pub)
	if e != nil {
		return e
	}
	if snap.Version < a.version || time.Since(time.Unix(snap.GeneratedAt, 0)) > 5*time.Minute || time.Unix(snap.GeneratedAt, 0).After(time.Now().Add(time.Minute)) {
		return fmt.Errorf("stale configuration")
	}
	if a.version == 0 || snap.Version != a.version {
		if e = a.App.Apply(snap); e != nil {
			return e
		}
	}
	if e = a.App.SaveSecret("agent_snapshot", string(control.Encode(signed))); e != nil {
		return e
	}
	a.version = snap.Version
	a.validUntil = time.Unix(snap.GeneratedAt, 0).Add(a.OfflineTTL)
	traffic, e := a.Store.Traffic()
	if e != nil {
		return e
	}
	sessions, e := a.Store.Sessions(100)
	if e != nil {
		return e
	}
	return a.request(ctx, "POST", "/api/v1/agent/heartbeat", control.Heartbeat{TrafficAccounting: "actual-v1", DeviceIdentitySupport: true, SubscriptionSupport: true, Sessions: sessions, Traffic: traffic, Active: a.App.Active(), Version: a.version}, nil)
}

// Upload independently of configuration and heartbeat reports so their failures
// can still be diagnosed from the control UI. Only acknowledged IDs are marked.
func (a *Agent) UploadDiagnostics(ctx context.Context) error {
	records, err := a.Store.PendingDiagnostics()
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return nil
	}
	// Leave room for JSON escaping and the signed API's request size limit.
	for len(records) > 1 && len(control.Encode(control.DiagnosticBatch{Records: records})) > 1<<20 {
		records = records[:len(records)-1]
	}
	var ack control.DiagnosticAck
	if err = a.request(ctx, "POST", "/api/v1/agent/diagnostics", control.DiagnosticBatch{Records: records}, &ack); err != nil {
		return err
	}
	sent := map[string]bool{}
	for _, d := range records {
		sent[d.ID] = true
	}
	for _, id := range ack.IDs {
		if !sent[id] {
			return fmt.Errorf("invalid diagnostic acknowledgement")
		}
	}
	return a.Store.AcknowledgeDiagnostics(ack.IDs)
}
func (a *Agent) Run(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	lastError := ""
	for {
		if err := a.UploadDiagnostics(ctx); err != nil {
			slog.Warn("agent diagnostic upload failed", "reason", err.Error())
		}
		if e := a.Sync(ctx); e != nil {
			if e.Error() != lastError {
				status := 503
				path := "/api/v1/agent/config"
				reason := "Agent 配置或心跳同步失败"
				code := "agent_sync_failed"
				if failure, ok := e.(*ControlHTTPError); ok {
					status = failure.Status
					if status < 400 || status > 599 {
						status = 502
					}
					path = failure.Path
					reason = fmt.Sprintf("Agent 同步：控制端 %s 返回 HTTP %d", path, failure.Status)
					code = "agent_control_http_error"
				} else if failure, ok := e.(*ControlRequestError); ok {
					path = failure.Path
					reason = "Agent 同步：控制端连接或响应失败"
				}
				recordErr := a.Store.RecordDiagnostic(control.Diagnostic{ID: control.ID(), Status: status, Reason: reason, CreatedAt: time.Now().Unix(), Method: map[bool]string{true: "POST", false: "GET"}[path == "/api/v1/agent/heartbeat"], Path: path, Source: "agent", Details: string(control.Encode(map[string]any{"code": code, "advice": "检查控制端对应接口的请求诊断、节点状态和配置版本"}))})
				if recordErr != nil {
					slog.Warn("agent synchronization diagnostic could not be saved")
				}
				lastError = e.Error()
			}
			slog.Warn("agent synchronization failed", "reason", e.Error())
			if time.Now().After(a.validUntil) {
				a.App.Apply(control.Snapshot{Version: a.version})
			}
		} else {
			lastError = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *Agent) acquireQuota(ctx context.Context, route control.Route, needed int64) error {
	value, _ := a.quotaLocks.LoadOrStore(route.ID, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	day := time.Now().UTC().Format("2006-01-02")
	grant, used, e := a.Store.AgentBudget(route.ID, day)
	if e != nil {
		return e
	}
	if grant-used >= needed {
		return nil
	}
	target := grant + 32*1024*1024
	if minimum := used + needed; target < minimum {
		target = minimum
	}
	if target > route.DailyQuota {
		target = route.DailyQuota
	}
	var out control.QuotaGrant
	if e = a.request(ctx, "POST", "/api/v1/agent/quota", control.QuotaRequest{RouteID: route.ID, Date: day, Target: target}, &out); e != nil {
		return e
	}
	if out.Date != day || out.Granted < grant {
		return fmt.Errorf("invalid quota grant")
	}
	return a.Store.SaveGrant(route.ID, out)
}
