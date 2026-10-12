package control

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"golang.org/x/crypto/bcrypt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type limiterEntry struct {
	Count int
	Since time.Time
}
type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]limiterEntry
}

func (l *loginLimiter) allow(addr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = map[string]limiterEntry{}
	}
	host, _, _ := net.SplitHostPort(addr)
	now := time.Now()
	for k, e := range l.entries {
		if now.Sub(e.Since) > 5*time.Minute {
			delete(l.entries, k)
		}
	}
	e := l.entries[host]
	if e.Since.IsZero() {
		e.Since = now
	}
	e.Count++
	if len(l.entries) > 10000 {
		return false
	}
	l.entries[host] = e
	return e.Count <= 10
}
func password(v string) (string, error) {
	if len(v) < 8 || len(v) > 72 {
		return "", fmt.Errorf("password must be 8–72 bytes")
	}
	b, e := bcrypt.GenerateFromPassword([]byte(v), bcrypt.DefaultCost)
	return string(b), e
}
func (s *Store) Bootstrap(username, pass string) error {
	var n int
	if e := s.DB.QueryRow("SELECT COUNT(*) FROM users").Scan(&n); e != nil {
		return e
	}
	if n > 0 {
		return nil
	}
	if username == "" {
		username = "admin"
	}
	hash, e := password(pass)
	if e != nil {
		return fmt.Errorf("first startup requires RELAY_ADMIN_PASSWORD: %w", e)
	}
	_, e = s.DB.Exec("INSERT INTO users(id,username,password_hash,role) VALUES(?,?,?,'admin')", ID(), username, hash)
	return e
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		apiError(w, 400, "invalid JSON body")
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		apiError(w, 400, "expected a single JSON object")
		return false
	}
	return true
}
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return r.Header.Get("Sec-Fetch-Site") != "cross-site"
	}
	u, e := url.Parse(origin)
	return e == nil && u.Host == r.Host && (u.Scheme == "http" || u.Scheme == "https")
}
func (a *App) current(r *http.Request) (User, error) {
	var u User
	c, e := r.Cookie("relay_session")
	if e != nil {
		return u, e
	}
	e = a.Store.DB.QueryRow(`SELECT u.id,u.username,u.role,u.password_hash,u.totp FROM login_sessions s JOIN users u ON u.id=s.user_id WHERE s.hash=? AND s.expires_at>?`, Hash(c.Value), time.Now().Unix()).Scan(&u.ID, &u.Username, &u.Role, &u.PasswordHash, &u.TOTP)
	return u, e
}
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if !a.limiter.allow(r.RemoteAddr) {
		apiError(w, 429, "too many attempts; retry in five minutes")
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if !decode(w, r, &in) {
		return
	}
	var u User
	e := a.Store.DB.QueryRow("SELECT id,username,role,password_hash,totp FROM users WHERE username=?", in.Username).Scan(&u.ID, &u.Username, &u.Role, &u.PasswordHash, &u.TOTP)
	if e == sql.ErrNoRows {
		a.portalLogin(w, r, in.Username, in.Password)
		return
	}
	hash := u.PasswordHash
	if e != nil {
		hash = a.dummyHash
	}
	ok := bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) == nil
	if e != nil || !ok {
		a.Store.Audit("anonymous", "login_failed", "")
		apiError(w, 401, "invalid credentials")
		return
	}
	if u.TOTP != "" {
		if in.Code == "" {
			writeJSON(w, http.StatusOK, map[string]bool{"totp_required": true})
			return
		}
		if !a.checkTOTP(u.TOTP, in.Code) {
			a.Store.Audit("anonymous", "login_failed", "")
			apiError(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
	}
	token := ID()
	if _, e = a.Store.DB.Exec("INSERT INTO login_sessions VALUES(?,?,?)", Hash(token), u.ID, time.Now().Add(12*time.Hour).Unix()); e != nil {
		apiError(w, 500, "session storage failed")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "relay_session", Value: token, Path: "/", HttpOnly: true, Secure: a.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
	if _, err := r.Cookie("relay_portal"); err == nil {
		http.SetCookie(w, &http.Cookie{Name: "relay_portal", Path: "/api/v1/portal", MaxAge: -1, HttpOnly: true, Secure: a.SecureCookies, SameSite: http.SameSiteStrictMode})
	}
	a.Store.Audit(u.ID, "login", u.ID)
	writeJSON(w, 200, u)
}
func (a *App) seal(v string) (string, error) {
	block, e := aes.NewCipher(a.master)
	if e != nil {
		return "", e
	}
	g, e := cipher.NewGCM(block)
	if e != nil {
		return "", e
	}
	nonce := make([]byte, g.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return "", e
	}
	return base64.RawStdEncoding.EncodeToString(g.Seal(nonce, nonce, []byte(v), []byte("EmbyRelay:v1"))), nil
}
func (a *App) unseal(v string) (string, error) {
	b, e := base64.RawStdEncoding.DecodeString(v)
	if e != nil {
		return "", e
	}
	block, e := aes.NewCipher(a.master)
	if e != nil {
		return "", e
	}
	g, e := cipher.NewGCM(block)
	if e != nil || len(b) < g.NonceSize() {
		return "", fmt.Errorf("invalid encrypted secret")
	}
	p, e := g.Open(nil, b[:g.NonceSize()], b[g.NonceSize():], []byte("EmbyRelay:v1"))
	return string(p), e
}
func totp(secret, code string, now time.Time) bool {
	key, e := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if e != nil || len(code) != 6 {
		return false
	}
	for offset := int64(-1); offset <= 1; offset++ {
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, uint64(now.Unix()/30+offset))
		h := hmac.New(sha1.New, key)
		h.Write(b)
		sum := h.Sum(nil)
		p := sum[len(sum)-1] & 15
		n := (binary.BigEndian.Uint32(sum[p:p+4]) & 0x7fffffff) % 1000000
		if subtle.ConstantTimeCompare([]byte(fmt.Sprintf("%06d", n)), []byte(code)) == 1 {
			return true
		}
	}
	return false
}
func (a *App) checkTOTP(encrypted, code string) bool {
	s, e := a.unseal(encrypted)
	return e == nil && totp(s, code, time.Now())
}
func intParam(r *http.Request, key string, def int) int {
	v, e := strconv.Atoi(r.URL.Query().Get(key))
	if e != nil || v < 1 || v > 1000 {
		return def
	}
	return v
}
func safeName(s string) bool {
	return len(s) > 0 && len(s) <= 80 && !strings.ContainsAny(s, "\r\n\x00")
}

func verifyPassword(hash, value string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(value)) == nil
}

// SaveSecret/LoadSecret protect locally cached configuration with the data-dir master key.
func (a *App) SaveSecret(key, value string) error {
	v, e := a.seal(value)
	if e != nil {
		return e
	}
	return a.Store.SetSetting(key, v)
}
func (a *App) LoadSecret(key string) (string, error) {
	v := a.Store.Setting(key)
	if v == "" {
		return "", nil
	}
	return a.unseal(v)
}

// ResetAdminPassword is an offline recovery operation for an existing administrator.
func (s *Store) ResetAdminPassword(username, pass string) error {
	hash, err := password(pass)
	if err != nil {
		return err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRow("SELECT id FROM users WHERE username=? AND role='admin'", username).Scan(&id); err != nil {
		return fmt.Errorf("administrator not found: %w", err)
	}
	if _, err := tx.Exec("UPDATE users SET password_hash=? WHERE id=?", hash, id); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM login_sessions WHERE user_id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}
