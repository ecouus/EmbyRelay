package control

import (
	"context"
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

func (l *loginLimiter) allow(scope, ip, username string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = map[string]limiterEntry{}
	}
	now := time.Now()
	for _, key := range limiterKeys(scope, ip, username) {
		entry := l.entries[key]
		if now.Sub(entry.Since) <= 5*time.Minute && entry.Count >= 10 {
			return false
		}
	}
	return true
}
func (l *loginLimiter) fail(scope, ip, username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = map[string]limiterEntry{}
	}
	now := time.Now()
	for _, key := range limiterFailureKeys(scope, ip, username) {
		entry := l.entries[key]
		if now.Sub(entry.Since) > 5*time.Minute {
			entry = limiterEntry{Since: now}
		}
		if entry.Since.IsZero() {
			entry.Since = now
		}
		entry.Count++
		l.entries[key] = entry
	}
	for len(l.entries) > 10000 {
		oldest := ""
		for key, entry := range l.entries {
			if oldest == "" || entry.Since.Before(l.entries[oldest].Since) {
				oldest = key
			}
		}
		delete(l.entries, oldest)
	}
}
func limiterKeys(scope, ip, username string) []string {
	keys := []string{scope + ":ip:" + ip}
	if username != "" {
		keys = append(keys, scope+":user-ip:"+strings.ToLower(username)+"@"+ip)
	}
	return keys
}
func limiterFailureKeys(scope, ip, username string) []string {
	keys := limiterKeys(scope, ip, username)
	if username != "" {
		keys = append(keys, scope+":user:"+strings.ToLower(username))
	}
	return keys
}

// Global username failures delay only failed responses, never valid credentials.
func (l *loginLimiter) failureDelay(scope, username string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.entries[scope+":user:"+strings.ToLower(username)]
	if username == "" || time.Since(entry.Since) > 5*time.Minute || entry.Count <= 50 {
		return 0
	}
	steps := min(8, 1+(entry.Count-51)/10)
	return time.Duration(steps) * 250 * time.Millisecond
}
func (l *loginLimiter) waitFailure(ctx context.Context, scope, username string) {
	delay := l.failureDelay(scope, username)
	if delay == 0 {
		return
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
func loginAuditTarget(username, ip string) string {
	if len(username) > 80 {
		username = username[:80]
	}
	username = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, username)
	return username + " @ " + ip
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
func authExpired(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": msg, "code": "auth_expired"})
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
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if !decode(w, r, &in) {
		return
	}
	ip := clientIP(r)
	if !a.limiter.allow("admin", ip, in.Username) {
		apiError(w, 429, "too many attempts; retry in five minutes")
		return
	}
	var u User
	e := a.Store.DB.QueryRow("SELECT id,username,role,password_hash,totp FROM users WHERE lower(username)=lower(?)", in.Username).Scan(&u.ID, &u.Username, &u.Role, &u.PasswordHash, &u.TOTP)
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
		a.limiter.fail("admin", ip, in.Username)
		a.limiter.waitFailure(r.Context(), "admin", in.Username)
		a.Store.Audit("anonymous", "login_failed", loginAuditTarget(in.Username, ip))
		apiError(w, 401, "invalid credentials")
		return
	}
	if u.TOTP != "" {
		if in.Code == "" {
			writeJSON(w, http.StatusOK, map[string]bool{"totp_required": true})
			return
		}
		if !a.consumeTOTP(u.ID, u.TOTP, in.Code) {
			a.limiter.fail("admin", ip, in.Username)
			a.limiter.waitFailure(r.Context(), "admin", in.Username)
			a.Store.Audit("anonymous", "login_failed", loginAuditTarget(in.Username, ip))
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
func totpStep(secret, code string, now time.Time) int64 {
	key, e := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if e != nil || len(code) != 6 {
		return -1
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
			return now.Unix()/30 + offset
		}
	}
	return -1
}
func totp(secret, code string, now time.Time) bool { return totpStep(secret, code, now) >= 0 }
func (a *App) checkTOTP(encrypted, code string) bool {
	s, e := a.unseal(encrypted)
	return e == nil && totp(s, code, time.Now())
}
func (a *App) consumeTOTP(userID, encrypted, code string) bool {
	secret, err := a.unseal(encrypted)
	if err != nil {
		return false
	}
	step := totpStep(secret, code, time.Now())
	if step < 0 {
		return false
	}
	result, err := a.Store.DB.Exec("INSERT INTO totp_replay(user_id,step) VALUES(?,?) ON CONFLICT(user_id) DO UPDATE SET step=excluded.step WHERE totp_replay.step<excluded.step", userID, step)
	if err != nil {
		return false
	}
	n, err := result.RowsAffected()
	return err == nil && n == 1
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
func (a *App) changeOwnPassword(w http.ResponseWriter, r *http.Request, user User) {
	var in struct {
		Current string `json:"current"`
		Next    string `json:"next"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !verifyPassword(user.PasswordHash, in.Current) {
		apiError(w, 401, "current password is incorrect")
		return
	}
	hash, err := password(in.Next)
	if err != nil {
		apiError(w, 400, err.Error())
		return
	}
	tx, err := a.Store.DB.Begin()
	if err != nil {
		apiError(w, 500, "storage unavailable")
		return
	}
	defer tx.Rollback()
	updated, err := tx.Exec("UPDATE users SET password_hash=? WHERE id=? AND password_hash=?", hash, user.ID, user.PasswordHash)
	if err != nil {
		apiError(w, 500, "storage unavailable")
		return
	}
	n, err := updated.RowsAffected()
	if err != nil || n != 1 {
		apiError(w, 409, "password changed; sign in again")
		return
	}
	if _, err = tx.Exec("DELETE FROM login_sessions WHERE user_id=?", user.ID); err != nil {
		apiError(w, 500, "storage unavailable")
		return
	}
	if _, err = tx.Exec("INSERT INTO audit_logs(actor_id,action,target,created_at) VALUES(?,?,?,?)", user.ID, "change_own_password", user.ID, time.Now().Unix()); err != nil {
		apiError(w, 500, "storage unavailable")
		return
	}
	if err = tx.Commit(); err != nil {
		apiError(w, 500, "storage unavailable")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "relay_session", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.SecureCookies, SameSite: http.SameSiteStrictMode})
	writeJSON(w, 200, map[string]bool{"ok": true})
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
	if err := tx.QueryRow("SELECT id FROM users WHERE lower(username)=lower(?) AND role='admin'", username).Scan(&id); err != nil {
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
