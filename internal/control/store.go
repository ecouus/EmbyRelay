package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embyrelay/internal/database"
	"embyrelay/migrations"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Upstream struct {
	DevicePolicy         *DevicePolicy `json:"device_policy,omitempty"`
	OwnerAccountID       string        `json:"owner_account_id,omitempty"`
	EncryptedCredentials string        `json:"encrypted_credentials,omitempty"`
	HasCredentials       bool          `json:"has_credentials,omitempty"`
	ID                   string        `json:"id"`
	Name                 string        `json:"name"`
	BaseURL              string        `json:"base_url"`
	Enabled              bool          `json:"enabled"`
	AllowedCIDRs         []string      `json:"allowed_cidrs"`
	RedirectHosts        []string      `json:"redirect_hosts"`
	Mode                 string        `json:"mode"`
}
type Route struct {
	OwnerAccountID string `json:"owner_account_id,omitempty"`
	ID             string `json:"id"`
	Alias          string `json:"alias"`
	UpstreamID     string `json:"upstream_id"`
	RelayID        string `json:"relay_id"`
	Enabled        bool   `json:"enabled"`
	RateBPS        int64  `json:"rate_bps"`
	DailyQuota     int64  `json:"daily_quota"`
	MaxConcurrent  int    `json:"max_concurrent"`
}
type Key struct {
	AccountID       string `json:"account_id,omitempty"`
	EncryptedSecret string `json:"encrypted_secret,omitempty"`
	AccessKey       string `json:"access_key,omitempty"`
	ID              string `json:"id"`
	RouteID         string `json:"route_id"`
	Hash            string `json:"hash,omitempty"`
	ExpiresAt       int64  `json:"expires_at"`
	Enabled         bool   `json:"enabled"`
}
type Node struct {
	DeviceIdentitySupport bool   `json:"device_identity_support"`
	SubscriptionSupport   bool   `json:"subscription_support"`
	ConfigVersion         int64  `json:"config_version"`
	Active                int64  `json:"active_sessions"`
	ID                    string `json:"id"`
	Name                  string `json:"name"`
	Endpoint              string `json:"endpoint"`
	PublicKey             string `json:"public_key,omitempty"`
	LastSeen              int64  `json:"last_seen"`
	Enabled               bool   `json:"enabled"`
}
type Snapshot struct {
	AutoMediaRedirect bool       `json:"auto_media_redirect,omitempty"`
	Version           int64      `json:"version"`
	GeneratedAt       int64      `json:"generated_at"`
	Upstreams         []Upstream `json:"upstreams"`
	Routes            []Route    `json:"routes"`
	Keys              []Key      `json:"keys"`
}
type User struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	Role         string `json:"role"`
	PasswordHash string `json:"-"`
	TOTP         string `json:"-"`
}
type Traffic struct {
	Date    string `json:"date"`
	RouteID string `json:"route_id"`
	Ingress int64  `json:"ingress_bytes"`
	Egress  int64  `json:"egress_bytes"`
}
type Store struct {
	AgentID string
	DB      *database.DB
	mu      sync.Mutex
}

func ID() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func Hash(v string) string { x := sha256.Sum256([]byte(v)); return hex.EncodeToString(x[:]) }
func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		f.Close()
		if err = os.Chmod(path, 0600); err != nil {
			return nil, err
		}
	}
	db, err := database.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = migrations.Apply(db.DB); err != nil {
		db.Close()
		return nil, err
	}

	db.Exec("UPDATE stream_sessions SET status='interrupted',ended_at=? WHERE status='active'", time.Now().Unix())
	return &Store{DB: db}, nil
}

// OpenConfigured preserves SQLite as the default for existing deployments.
func OpenConfigured(dir string) (*Store, error) {
	driver := os.Getenv("RELAY_DB_DRIVER")
	if driver == "" || driver == "sqlite" {
		return Open(filepath.Join(dir, "embyrelay.db"))
	}
	if driver != "postgres" {
		return nil, fmt.Errorf("unsupported RELAY_DB_DRIVER")
	}
	source := os.Getenv("RELAY_DATABASE_URL")
	if file := os.Getenv("RELAY_DATABASE_URL_FILE"); file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read database credential file: %w", err)
		}
		source = strings.TrimSpace(string(raw))
	}
	if source == "" {
		return nil, fmt.Errorf("PostgreSQL requires RELAY_DATABASE_URL or RELAY_DATABASE_URL_FILE")
	}
	return openPostgres(source)
}

func openPostgres(source string) (*Store, error) {
	db, err := database.Open("postgres", source)
	if err != nil {
		return nil, fmt.Errorf("invalid PostgreSQL configuration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		attempt, end := context.WithTimeout(ctx, 3*time.Second)
		err = db.PingContext(attempt)
		end()
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			db.Close()
			return nil, fmt.Errorf("PostgreSQL connection unavailable after 30 seconds")
		case <-time.After(time.Second):
		}
	}
	if err = migrations.ApplyPostgres(db.DB); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec("UPDATE stream_sessions SET status='interrupted',ended_at=? WHERE status='active'", time.Now().Unix()); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: db}, nil
}
func list[T any](s *Store, kind string) ([]T, error) {
	rows, e := s.DB.Query("SELECT payload FROM objects WHERE kind=? ORDER BY updated_at,id", kind)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var raw string
		var v T
		if e = rows.Scan(&raw); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(raw), &v); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func get[T any](s *Store, kind, id string) (T, error) {
	var out T
	var raw string
	e := s.DB.QueryRow("SELECT payload FROM objects WHERE kind=? AND id=?", kind, id).Scan(&raw)
	if e == nil {
		e = json.Unmarshal([]byte(raw), &out)
	}
	return out, e
}
func (s *Store) put(kind, id string, v any, actor string) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec("INSERT INTO objects VALUES(?,?,?,?) ON CONFLICT(kind,id) DO UPDATE SET payload=excluded.payload,updated_at=excluded.updated_at", kind, id, string(b), time.Now().Unix()); e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE settings SET value=CAST(value AS INTEGER)+1 WHERE key='version'"); e != nil {
		return e
	}
	if _, e = tx.Exec("INSERT INTO audit_logs(actor_id,action,target,created_at) VALUES(?,?,?,?)", actor, "save_"+kind, id, time.Now().Unix()); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) remove(kind, id, actor string) error {
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec("DELETE FROM objects WHERE kind=? AND id=?", kind, id); e != nil {
		return e
	}
	if kind == "upstream" {
		if _, e = tx.Exec("DELETE FROM access_devices WHERE upstream_id=?", id); e != nil {
			return e
		}
		if _, e = tx.Exec("DELETE FROM objects WHERE kind='device_policy' AND id LIKE ?", "%:"+id); e != nil {
			return e
		}
	}
	if kind == "route" {
		if _, e = tx.Exec("DELETE FROM objects WHERE kind='key' AND json_extract(payload,'$.route_id')=?", id); e != nil {
			return e
		}
	}
	tx.Exec("UPDATE settings SET value=CAST(value AS INTEGER)+1 WHERE key='version'")
	tx.Exec("INSERT INTO audit_logs(actor_id,action,target,created_at) VALUES(?,?,?,?)", actor, "delete_"+kind, id, time.Now().Unix())
	return tx.Commit()
}
func (s *Store) Snapshot() (Snapshot, error) {
	var out Snapshot
	var err error
	out.GeneratedAt = time.Now().Unix()
	out.AutoMediaRedirect = true
	if err = s.DB.QueryRow("SELECT value FROM settings WHERE key='version'").Scan(&out.Version); err != nil {
		return out, err
	}
	if out.Upstreams, err = list[Upstream](s, "upstream"); err != nil {
		return out, err
	}
	for i := range out.Upstreams {
		out.Upstreams[i].EncryptedCredentials = ""
		out.Upstreams[i].HasCredentials = false
		out.Upstreams[i].Mode = "proxy"
	}
	if out.Routes, err = list[Route](s, "route"); err != nil {
		return out, err
	}
	out.Keys, err = list[Key](s, "key")
	for i := range out.Keys {
		out.Keys[i].EncryptedSecret = ""
		out.Keys[i].AccessKey = ""
	}
	return out, err
}

var ErrQuota = errors.New("daily quota exhausted")

func (s *Store) Charge(route string, quota, ingress, egress int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	date := time.Now().UTC().Format("2006-01-02")
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec("INSERT INTO traffic_daily(date,route_id) VALUES(?,?) ON CONFLICT DO NOTHING", date, route); e != nil {
		return e
	}
	var used int64
	if e = tx.QueryRow("SELECT egress_bytes FROM traffic_daily WHERE date=? AND route_id=?"+tx.ForUpdate(), date, route).Scan(&used); e != nil {
		return e
	}
	if quota > 0 && egress > 0 {
		if s.AgentID != "" {
			var grant int64
			err := tx.QueryRow("SELECT granted FROM quota_leases WHERE date=? AND route_id=? AND node_id=?", date, route, s.AgentID).Scan(&grant)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			quota = grant
		} else {
			var reserved int64
			if err := tx.QueryRow("SELECT COALESCE(SUM(MAX(0,granted-reported)),0) FROM quota_leases WHERE date=? AND route_id=?", date, route).Scan(&reserved); err != nil {
				return err
			}
			if reserved > quota-used {
				return ErrQuota
			}
			used += reserved
		}
		if used >= quota || egress > quota-used {
			return ErrQuota
		}
	}
	if quota > 0 && egress > 0 && (used >= quota || egress > quota-used) {
		return ErrQuota
	}
	if _, e = tx.Exec("UPDATE traffic_daily SET ingress_bytes=MAX(0,ingress_bytes+?),egress_bytes=MAX(0,egress_bytes+?) WHERE date=? AND route_id=?", ingress, egress, date, route); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) Traffic() ([]Traffic, error) {
	rows, e := s.DB.Query("SELECT date,route_id,ingress_bytes,egress_bytes FROM traffic_daily ORDER BY date DESC LIMIT 1000")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Traffic{}
	for rows.Next() {
		var x Traffic
		if e = rows.Scan(&x.Date, &x.RouteID, &x.Ingress, &x.Egress); e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func (s *Store) Setting(key string) string {
	var v string
	s.DB.QueryRow("SELECT value FROM settings WHERE key=?", key).Scan(&v)
	return v
}
func (s *Store) SetSetting(key, value string) error {
	_, e := s.DB.Exec("INSERT INTO settings VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	return e
}
func (s *Store) Audit(actor, action, target string) error {
	_, e := s.DB.Exec("INSERT INTO audit_logs(actor_id,action,target,created_at) VALUES(?,?,?,?)", actor, action, target, time.Now().Unix())
	return e
}
func (s *Store) Cleanup() {
	now := time.Now().Unix()
	s.DB.Exec("DELETE FROM request_diagnostics WHERE node_id='' AND id NOT IN (SELECT id FROM request_diagnostics WHERE node_id='' ORDER BY created_at DESC,id DESC LIMIT 1000)")
	s.DB.Exec("DELETE FROM audit_logs WHERE created_at<?", now-365*86400)
	s.DB.Exec("DELETE FROM traffic_daily WHERE date<?", time.Unix(now-365*86400, 0).UTC().Format("2006-01-02"))
	s.DB.Exec("DELETE FROM quota_leases WHERE date<?", time.Unix(now-90*86400, 0).UTC().Format("2006-01-02"))
	s.DB.Exec("DELETE FROM remote_sessions WHERE updated_at<?", now-30*86400)
	s.DB.Exec("DELETE FROM login_sessions WHERE expires_at<?", now)
	s.DB.Exec("DELETE FROM access_tokens WHERE expires_at<=?", now)
	s.DB.Exec("DELETE FROM portal_sessions WHERE expires_at<=?", now)
	// Authorization still expires after 30 seconds. Keep expired leases briefly
	// so a delayed finish can refund the cycle in which its last chunk was charged.
	s.DB.Exec("DELETE FROM access_leases WHERE expires_at<=?", now-int64(accessSettlementGrace/time.Second))
	if err := s.cleanupAccessUsage(time.Unix(now, 0)); err != nil {
		slog.Error("subscription usage cleanup failed", "error", err)
	}
	s.DB.Exec("DELETE FROM access_targets WHERE expires_at<=? OR NOT EXISTS (SELECT 1 FROM access_tokens WHERE hash=access_targets.token_hash)", now)
	s.DB.Exec("DELETE FROM access_devices WHERE last_seen<?", now-90*86400)
	s.DB.Exec("DELETE FROM nonces WHERE created_at<?", now-600)
	s.DB.Exec("DELETE FROM enroll_tokens WHERE expires_at<?", now)
	s.DB.Exec("DELETE FROM stream_sessions WHERE ended_at<?", now-30*86400)
}
func (s *Store) Check() error   { return s.DB.Ping() }
func (s *Store) Close() error   { return s.DB.Close() }
func (s *Store) String() string { return fmt.Sprint("SQLite control store") }
