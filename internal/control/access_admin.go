package control

import (
	"fmt"
	"net/http"
	"time"
)

func (a *App) accessAdmin(w http.ResponseWriter, r *http.Request, user User, resource, id string, parts []string) {
	if user.Role != "admin" {
		apiError(w, 403, "administrator role required")
		return
	}
	if resource == "access-plans" {
		if r.Method == "GET" {
			v, e := list[AccessPlan](a.Store, "access_plan")
			if e != nil {
				apiError(w, 500, "storage unavailable")
				return
			}
			writeJSON(w, 200, v)
			return
		}
		if r.Method != "POST" && r.Method != "PATCH" {
			apiError(w, 405, "method not allowed")
			return
		}
		var p AccessPlan
		if !decode(w, r, &p) {
			return
		}
		if e := validPlan(p); e != nil {
			apiError(w, 400, e.Error())
			return
		}
		if id == "" {
			id = ID()
		} else if _, e := get[AccessPlan](a.Store, "access_plan", id); e != nil {
			apiError(w, 404, "plan not found")
			return
		}
		p.ID = id
		a.mutate(w, func() error { return a.Store.put("access_plan", id, p, user.ID) })
		return
	}
	if len(parts) == 3 && parts[2] == "sessions" && r.Method == "GET" {
		a.queryRows(w, "SELECT hash,key_id,device,client_ip,user_agent,created_at,expires_at FROM access_tokens WHERE account_id=? ORDER BY created_at DESC LIMIT 100", id)
		return
	}
	if r.Method == "GET" {
		cs, e := list[AccessAccount](a.Store, "access_account")
		if e != nil {
			apiError(w, 500, "storage unavailable")
			return
		}
		out := []map[string]any{}
		for _, c := range cs {
			c.PasswordHash = ""
			c.Credentials = ""
			cycle, next := accountCycle(c, time.Now())
			var used int64
			a.Store.DB.QueryRow("SELECT bytes FROM access_usage WHERE account_id=? AND cycle=?", c.ID, cycle).Scan(&used)
			remaining := int64(-1)
			if c.ExpiresAt > 0 {
				remaining = max(int64(0), (c.ExpiresAt-time.Now().Unix()+86399)/86400)
			}
			out = append(out, map[string]any{"account": c, "used_bytes": used, "remaining_days": remaining, "next_reset_at": next})
		}
		writeJSON(w, 200, out)
		return
	}
	if id != "" && len(parts) == 3 {
		var in struct {
			Days     int    `json:"days"`
			Months   int    `json:"months"`
			Password string `json:"password"`
			Enabled  *bool  `json:"enabled"`
		}
		if r.Method != "POST" {
			apiError(w, 405, "method not allowed")
			return
		}
		if !decode(w, r, &in) {
			return
		}
		action := parts[2]
		if action != "renew" && action != "reset-usage" && action != "revoke-sessions" && action != "status" && action != "password" {
			apiError(w, 404, "action not found")
			return
		}
		var hash string
		var err error
		if action == "password" {
			hash, err = password(in.Password)
			if err != nil {
				apiError(w, 400, err.Error())
				return
			}
		}
		a.mutate(w, func() error {
			a.accessMu.Lock()
			defer a.accessMu.Unlock()
			tx, e := a.Store.DB.Begin()
			if e != nil {
				return e
			}
			defer tx.Rollback()
			c, e := loadAccount(tx, id)
			if e != nil {
				return fmt.Errorf("account not found")
			}
			switch action {
			case "renew":
				if in.Days < 0 || in.Months < 0 || in.Days > 36500 || in.Months > 1200 || (in.Days > 0) == (in.Months > 0) {
					return fmt.Errorf("specify either positive days or months")
				}
				start := max(time.Now().Unix(), c.ExpiresAt)
				if c.StartedAt == 0 {
					c.StartedAt = time.Now().Unix()
				}
				c.ExpiresAt = planExpiry(start, AccessPlan{DurationDays: in.Days, DurationMonths: in.Months})
			case "reset-usage":
				c.ResetAt = time.Now().UnixNano()
			case "status":
				if in.Enabled == nil {
					return fmt.Errorf("enabled required")
				}
				c.Enabled = *in.Enabled
			case "password":
				c.PasswordHash = hash
			}
			if action == "password" || action == "revoke-sessions" || action == "status" && !c.Enabled {
				if _, e = tx.Exec("DELETE FROM portal_sessions WHERE account_id=?", id); e != nil {
					return e
				}
				if _, e = tx.Exec("DELETE FROM access_tokens WHERE account_id=?", id); e != nil {
					return e
				}
				if _, e = tx.Exec("DELETE FROM access_leases WHERE account_id=?", id); e != nil {
					return e
				}
			}
			if e = saveAccount(tx, c); e != nil {
				return e
			}
			if _, e = tx.Exec("INSERT INTO audit_logs(actor_id,action,target,created_at) VALUES(?,?,?,?)", user.ID, "subscription_"+action, id, time.Now().Unix()); e != nil {
				return e
			}
			return tx.Commit()
		})
		return
	}
	if r.Method != "POST" || id != "" {
		apiError(w, 405, "method not allowed")
		return
	}
	var in struct {
		RouteID        string `json:"route_id"`
		Username       string `json:"username"`
		Password       string `json:"password"`
		PlanID         string `json:"plan_id"`
		StartMode      string `json:"start_mode"`
		ExpiresAt      int64  `json:"expires_at"`
		SourceUsername string `json:"source_username"`
		SourcePassword string `json:"source_password"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !safeName(in.Username) || (in.StartMode != "immediate" && in.StartMode != "first_login") || in.ExpiresAt < 0 {
		apiError(w, 400, "invalid subscription")
		return
	}
	plan, e := get[AccessPlan](a.Store, "access_plan", in.PlanID)
	if e != nil {
		apiError(w, 400, "plan not found")
		return
	}
	var route Route
	if in.RouteID != "" {
		route, e = get[Route](a.Store, "route", in.RouteID)
		if e != nil || route.OwnerAccountID != "" {
			apiError(w, 400, "请选择公共路由，或留空让用户自行添加源站")
			return
		}
	}
	hash, e := password(in.Password)
	if e != nil {
		apiError(w, 400, e.Error())
		return
	}
	c := AccessAccount{ID: ID(), RouteID: in.RouteID, KeyID: ID(), Username: in.Username, PasswordHash: hash, PlanID: in.PlanID, Plan: plan, StartMode: in.StartMode, Enabled: true, ExpiresAt: in.ExpiresAt}
	if in.StartMode == "immediate" {
		c.StartedAt = time.Now().Unix()
		if c.ExpiresAt == 0 {
			c.ExpiresAt = planExpiry(c.StartedAt, plan)
		}
	}
	if c.ExpiresAt > 0 && c.ExpiresAt <= time.Now().Unix() {
		apiError(w, 400, "expiry must be in future")
		return
	}
	if in.RouteID == "" {
		c.KeyID = ""
		if in.SourceUsername != "" || in.SourcePassword != "" {
			apiError(w, 400, "未分配路由时无需填写源站凭据")
			return
		}
	} else if in.SourceUsername != "" || in.SourcePassword != "" {
		if in.SourceUsername == "" || in.SourcePassword == "" || len(in.SourceUsername) > 1024 || len(in.SourcePassword) > 4096 {
			apiError(w, 400, "source credentials incomplete")
			return
		}
		c.Credentials, e = a.seal(string(Encode(embyCredentials{in.SourceUsername, in.SourcePassword})))
	} else {
		up, err := get[Upstream](a.Store, "upstream", route.UpstreamID)
		if err != nil || up.EncryptedCredentials == "" {
			apiError(w, 400, "先设置源站账号或填写独立源站账号")
			return
		}
	}
	if e != nil {
		apiError(w, 500, "credential encryption failed")
		return
	}
	secret := ID()
	encrypted, e := a.seal(secret)
	if e != nil {
		apiError(w, 500, "key encryption failed")
		return
	}
	k := Key{ID: c.KeyID, RouteID: c.RouteID, AccountID: c.ID, Hash: Hash(secret), EncryptedSecret: encrypted, Enabled: true}
	a.configMu.Lock()
	defer a.configMu.Unlock()
	tx, e := a.Store.DB.Begin()
	if e != nil {
		apiError(w, 500, "storage unavailable")
		return
	}
	defer tx.Rollback()
	var existing int
	if e = tx.QueryRow("SELECT COUNT(*) FROM objects WHERE kind='access_account' AND lower(json_extract(payload,'$.username'))=lower(?)", c.Username).Scan(&existing); e != nil || existing != 0 {
		apiError(w, 400, "用户名已存在或不可用")
		return
	}
	if e = saveAccount(tx, c); e == nil && c.KeyID != "" {
		_, e = tx.Exec("INSERT INTO objects VALUES('key',?,?,?)", k.ID, string(Encode(k)), time.Now().Unix())
	}
	if e == nil {
		_, e = tx.Exec("UPDATE settings SET value=CAST(value AS INTEGER)+1 WHERE key='version'")
	}
	if e == nil {
		_, e = tx.Exec("INSERT INTO audit_logs(actor_id,action,target,created_at) VALUES(?,?,?,?)", user.ID, "create_access_account", c.ID, time.Now().Unix())
	}
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		apiError(w, 500, "subscription creation failed")
		return
	}
	if e = a.Reload(); e != nil {
		apiError(w, 500, "reload failed")
		return
	}
	if c.KeyID == "" {
		secret = ""
	}
	writeJSON(w, 201, map[string]string{"id": c.ID, "username": c.Username, "access_key": secret, "route_alias": route.Alias})
}
