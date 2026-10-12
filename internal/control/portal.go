package control

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (a *App) portal(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !sameOrigin(r) || (r.Method != "GET" && r.Header.Get("X-Requested-With") != "EmbyRelay") {
		apiError(w, 403, "请求来源无效")
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/api/v1/portal/")
	if p == "login" && r.Method == "POST" {
		var in struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if !decode(w, r, &in) {
			return
		}
		a.portalLogin(w, r, in.Username, in.Password)
		return
	}
	cookie, e := r.Cookie("relay_portal")
	if e != nil {
		apiError(w, 401, "请登录用户中心")
		return
	}
	var id string
	if e = a.Store.DB.QueryRow("SELECT account_id FROM portal_sessions WHERE hash=? AND expires_at>?", Hash(cookie.Value), time.Now().Unix()).Scan(&id); e != nil {
		apiError(w, 401, "请重新登录")
		return
	}
	c, e := get[AccessAccount](a.Store, "access_account", id)
	if e != nil || !c.Enabled {
		apiError(w, 401, "账号不可用")
		return
	}
	if p == "logout" && r.Method == "POST" {
		a.Store.DB.Exec("DELETE FROM portal_sessions WHERE hash=?", Hash(cookie.Value))
		http.SetCookie(w, &http.Cookie{Name: "relay_portal", Path: "/api/v1/portal", MaxAge: -1, HttpOnly: true, Secure: a.SecureCookies, SameSite: http.SameSiteStrictMode})
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	if p == "me" && r.Method == "GET" {
		cycle, next := accountCycle(c, time.Now())
		var used int64
		e = a.Store.DB.QueryRow("SELECT bytes FROM access_usage WHERE account_id=? AND cycle=?", c.ID, cycle).Scan(&used)
		if e != nil && e != sql.ErrNoRows {
			apiError(w, 500, "流量读取失败")
			return
		}
		c.PasswordHash = ""
		c.Credentials = ""
		writeJSON(w, 200, map[string]any{"account": c, "used_bytes": used, "next_reset_at": next})
		return
	}
	if p == "devices" && r.Method == "GET" {
		a.deviceRecords(w, r, c.ID)
		return
	}
	parts := strings.Split(p, "/")
	if parts[0] != "sources" || len(parts) > 2 {
		apiError(w, 404, "接口不存在")
		return
	}
	sourceID := ""
	if len(parts) == 2 {
		sourceID = parts[1]
	}
	if r.Method == "GET" && sourceID == "" {
		a.portalSources(w, c)
		return
	}
	if r.Method != "POST" && r.Method != "PATCH" && r.Method != "DELETE" {
		apiError(w, 405, "method not allowed")
		return
	}
	if (r.Method == "POST") != (sourceID == "") {
		apiError(w, 400, "源站地址无效")
		return
	}
	var in struct {
		Name     string  `json:"name"`
		BaseURL  string  `json:"base_url"`
		Username *string `json:"emby_username"`
		Password *string `json:"emby_password"`
	}
	if r.Method != "DELETE" && !decode(w, r, &in) {
		return
	}
	a.mutate(w, func() error {
		a.accessMu.Lock()
		defer a.accessMu.Unlock()
		fresh, err := get[AccessAccount](a.Store, "access_account", c.ID)
		if err != nil || !fresh.Enabled {
			return fmt.Errorf("账号不可用")
		}
		var u Upstream
		if sourceID != "" {
			u, err = get[Upstream](a.Store, "upstream", sourceID)
			if err != nil || u.OwnerAccountID != c.ID {
				return fmt.Errorf("源站不存在")
			}
		}
		if r.Method != "DELETE" {
			if err = accountActive(fresh, time.Now()); err != nil {
				return err
			}
			normalized, e := normalizeUpstreamURL(in.BaseURL)
			if e != nil {
				return e
			}
			if sourceID == "" {
				u = Upstream{ID: ID(), OwnerAccountID: c.ID, Enabled: true, Mode: "proxy"}
			}
			u.Name = in.Name
			u.BaseURL = normalized
			if e = validateUpstream(u); e != nil {
				return e
			}
			// Users cannot supply private-network exceptions or choose other users' routes.
			if in.Password != nil && *in.Password == "" {
				in.Password = nil
			}
			if e = a.updateCredentials(&u, in.Username, in.Password, false); e != nil {
				return e
			}
			var creds embyCredentials
			raw, e := a.unseal(u.EncryptedCredentials)
			if e != nil || json.Unmarshal([]byte(raw), &creds) != nil || creds.Username == "" || creds.Password == "" {
				return fmt.Errorf("请填写 Emby 用户名和密码")
			}
		}
		tx, e := a.Store.DB.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if sourceID == "" {
			var n int
			if e = tx.QueryRow("SELECT COUNT(*) FROM objects WHERE kind='upstream' AND json_extract(payload,'$.owner_account_id')=?", c.ID).Scan(&n); e != nil {
				return e
			}
			if n >= 50 {
				return fmt.Errorf("最多添加 50 个源站")
			}
			route := Route{ID: ID(), Alias: "u-" + ID()[:20], UpstreamID: u.ID, OwnerAccountID: c.ID, RelayID: "auto", Enabled: true, MaxConcurrent: 1000}
			secret := ID()
			sealed, e := a.seal(secret)
			if e != nil {
				return e
			}
			key := Key{ID: ID(), RouteID: route.ID, AccountID: c.ID, Hash: Hash(secret), EncryptedSecret: sealed, Enabled: true}
			for _, obj := range []struct {
				kind, id string
				value    any
			}{{"route", route.ID, route}, {"key", key.ID, key}} {
				if _, e = tx.Exec("INSERT INTO objects VALUES(?,?,?,?)", obj.kind, obj.id, string(Encode(obj.value)), time.Now().Unix()); e != nil {
					return e
				}
			}
		} else {
			// Invalidate playback sessions whenever this source changes; shared usage survives.
			if _, e = tx.Exec("DELETE FROM access_tokens WHERE key_id IN (SELECT id FROM objects WHERE kind='key' AND json_extract(payload,'$.route_id') IN (SELECT id FROM objects WHERE kind='route' AND json_extract(payload,'$.upstream_id')=?))", u.ID); e != nil {
				return e
			}
		}
		if r.Method == "DELETE" {
			if _, e = tx.Exec("DELETE FROM access_devices WHERE upstream_id=?", u.ID); e != nil {
				return e
			}
			if _, e = tx.Exec("DELETE FROM objects WHERE kind='device_policy' AND id LIKE ?", "%:"+u.ID); e != nil {
				return e
			}
			for _, query := range []string{"DELETE FROM objects WHERE kind='key' AND json_extract(payload,'$.route_id') IN (SELECT id FROM objects WHERE kind='route' AND json_extract(payload,'$.upstream_id')=?)", "DELETE FROM objects WHERE kind='route' AND json_extract(payload,'$.upstream_id')=?", "DELETE FROM objects WHERE kind='upstream' AND id=?"} {
				if _, e = tx.Exec(query, u.ID); e != nil {
					return e
				}
			}
		} else {
			if _, e = tx.Exec("INSERT INTO objects VALUES('upstream',?,?,?) ON CONFLICT(kind,id) DO UPDATE SET payload=excluded.payload,updated_at=excluded.updated_at", u.ID, string(Encode(u)), time.Now().Unix()); e != nil {
				return e
			}
		}
		if _, e = tx.Exec("UPDATE settings SET value=CAST(value AS INTEGER)+1 WHERE key='version'"); e != nil {
			return e
		}
		if _, e = tx.Exec("INSERT INTO audit_logs(actor_id,action,target,created_at) VALUES(?,?,?,?)", c.ID, "portal_source_"+r.Method, u.ID, time.Now().Unix()); e != nil {
			return e
		}
		return tx.Commit()
	})
}

func (a *App) portalSources(w http.ResponseWriter, c AccessAccount) {
	us, e := list[Upstream](a.Store, "upstream")
	if e != nil {
		apiError(w, 500, "读取失败")
		return
	}
	rs, e := list[Route](a.Store, "route")
	if e != nil {
		apiError(w, 500, "读取失败")
		return
	}
	ks, e := list[Key](a.Store, "key")
	if e != nil {
		apiError(w, 500, "读取失败")
		return
	}
	nodes, e := list[Node](a.Store, "node")
	if e != nil {
		apiError(w, 500, "读取失败")
		return
	}
	byNode := map[string]Node{}
	for _, node := range nodes {
		byNode[node.ID] = node
	}
	configVersion := a.state.Load().snapshot.Version
	now := time.Now().Unix()
	out := []map[string]any{}
	for _, u := range us {
		if u.OwnerAccountID != c.ID {
			continue
		}
		item := map[string]any{"id": u.ID, "name": u.Name, "base_url": u.BaseURL, "enabled": u.Enabled, "addresses": []string{}}
		paths := []string{}
		connections := []UserSourceAddress{}
		for _, route := range rs {
			if route.UpstreamID != u.ID || route.OwnerAccountID != c.ID || !route.Enabled {
				continue
			}
			for _, k := range ks {
				if k.RouteID == route.ID && k.AccountID == c.ID && k.Enabled && (k.ExpiresAt == 0 || k.ExpiresAt > time.Now().Unix()) {
					secret, e := a.unseal(k.EncryptedSecret)
					if e != nil {
						apiError(w, 500, "地址解密失败")
						return
					}
					path := "/r/" + route.Alias + "/" + secret + "/"
					paths = append(paths, path)
					origin, label := subscriptionAddressEntry(route, byNode, configVersion, now)
					connections = append(connections, UserSourceAddress{RouteID: route.ID, RouteAlias: route.Alias, Path: path, Origin: origin, NodeID: route.RelayID, EntryLabel: label, Available: true})
				}
			}
		}
		item["addresses"] = paths
		item["connection_addresses"] = connections
		out = append(out, item)
	}
	writeJSON(w, 200, out)
}

// portalLogin authenticates a subscriber after the caller applies rate limiting.
func (a *App) portalLogin(w http.ResponseWriter, r *http.Request, username, password string) {
	ip := clientIP(r)
	if !a.limiter.allow("portal", ip, username) {
		apiError(w, 429, "尝试过多，请稍后重试")
		return
	}
	var raw string
	rows, err := a.Store.DB.Query("SELECT payload FROM objects WHERE kind='access_account' AND lower(json_extract(payload,'$.username'))=lower(?)", username)
	if err != nil {
		apiError(w, 500, "账号查询失败")
		return
	}
	count := 0
	for rows.Next() {
		count++
		if err = rows.Scan(&raw); err != nil {
			break
		}
	}
	rows.Close()
	var c AccessAccount
	json.Unmarshal([]byte(raw), &c)
	hash := c.PasswordHash
	if count != 1 {
		hash = a.dummyHash
	}
	if !verifyPassword(hash, password) || count != 1 || err != nil || !c.Enabled {
		a.limiter.fail("portal", ip, username)
		apiError(w, 401, "用户名或密码错误，或账号已停用")
		return
	}
	a.accessMu.Lock()
	defer a.accessMu.Unlock()
	fresh, e := get[AccessAccount](a.Store, "access_account", c.ID)
	if e != nil || !fresh.Enabled || fresh.PasswordHash != c.PasswordHash {
		apiError(w, 401, "账号已变化，请重新登录")
		return
	}
	token := ID()
	if _, e = a.Store.DB.Exec("INSERT INTO portal_sessions VALUES(?,?,?)", Hash(token), c.ID, time.Now().Add(12*time.Hour).Unix()); e != nil {
		apiError(w, 500, "登录失败")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "relay_portal", Value: token, Path: "/api/v1/portal", HttpOnly: true, Secure: a.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
	if _, err := r.Cookie("relay_session"); err == nil {
		http.SetCookie(w, &http.Cookie{Name: "relay_session", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.SecureCookies, SameSite: http.SameSiteStrictMode})
	}
	writeJSON(w, 200, map[string]any{"ok": true, "role": "subscriber"})
}
