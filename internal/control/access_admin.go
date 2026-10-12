package control

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (a *App) accessAdmin(w http.ResponseWriter, r *http.Request, user User, resource, id string, parts []string) {
	if user.Role != "admin" {
		apiError(w, 403, "administrator role required")
		return
	}
	if resource == "access-plans" {
		if r.Method == "POST" && id != "" && len(parts) == 3 && parts[2] == "apply" {
			a.mutate(w, func() error {
				a.accessMu.Lock()
				defer a.accessMu.Unlock()
				plan, err := get[AccessPlan](a.Store, "access_plan", id)
				if err != nil {
					return fmt.Errorf("plan not found")
				}
				accounts, err := list[AccessAccount](a.Store, "access_account")
				if err != nil {
					return err
				}
				tx, err := a.Store.DB.Begin()
				if err != nil {
					return err
				}
				defer tx.Rollback()
				for _, account := range accounts {
					if account.PlanID != id {
						continue
					}
					account.Plan = plan
					if _, err = tx.Exec("UPDATE objects SET payload=?,updated_at=? WHERE kind='access_account' AND id=?", string(Encode(account)), time.Now().Unix(), account.ID); err != nil {
						return err
					}
				}
				if _, err = tx.Exec("INSERT INTO audit_logs(actor_id,action,target,created_at) VALUES(?,?,?,?)", user.ID, "apply_access_plan", id, time.Now().Unix()); err != nil {
					return err
				}
				return tx.Commit()
			})
			return
		}
		if r.Method == "GET" {
			v, e := list[AccessPlan](a.Store, "access_plan")
			if e != nil {
				apiError(w, 500, "storage unavailable")
				return
			}
			writeJSON(w, 200, v)
			return
		}
		if r.Method == "DELETE" && id != "" {
			a.mutate(w, func() error {
				var count int
				if err := a.Store.DB.QueryRow("SELECT COUNT(*) FROM objects WHERE kind='access_account' AND json_extract(payload,'$.plan_id')=?", id).Scan(&count); err != nil {
					return err
				}
				if count > 0 {
					return fmt.Errorf("套餐仍有已开通账号，无法删除")
				}
				return a.Store.remove("access_plan", id, user.ID)
			})
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
	if resource == "access-accounts" && r.Method == "DELETE" && id != "" && len(parts) == 2 {
		a.mutate(w, func() error {
			a.accessMu.Lock()
			defer a.accessMu.Unlock()
			tx, err := a.Store.DB.Begin()
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if _, err = loadAccount(tx, id); err != nil {
				return fmt.Errorf("账号不存在")
			}
			for _, table := range []string{"portal_sessions", "access_tokens", "access_leases", "access_targets", "access_usage", "access_rate", "access_devices"} {
				if _, err = tx.Exec("DELETE FROM "+table+" WHERE account_id=?", id); err != nil {
					return err
				}
			}
			for _, kind := range []string{"key", "route", "upstream"} {
				if _, err = tx.Exec("DELETE FROM objects WHERE kind=? AND json_extract(payload,'$.owner_account_id')=?", kind, id); err != nil {
					return err
				}
			}
			if _, err = tx.Exec("DELETE FROM objects WHERE kind='key' AND json_extract(payload,'$.account_id')=?", id); err != nil {
				return err
			}
			if _, err = tx.Exec("DELETE FROM objects WHERE kind='access_account' AND id=?", id); err != nil {
				return err
			}
			if _, err = tx.Exec("INSERT INTO audit_logs(actor_id,action,target,created_at) VALUES(?,?,?,?)", user.ID, "subscription_delete", id, time.Now().Unix()); err != nil {
				return err
			}
			return tx.Commit()
		})
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
		routes, e := list[Route](a.Store, "route")
		if e != nil {
			apiError(w, 500, "storage unavailable")
			return
		}
		nodes, e := list[Node](a.Store, "node")
		if e != nil {
			apiError(w, 500, "storage unavailable")
			return
		}
		byRoute := map[string]Route{}
		byNode := map[string]Node{}
		for _, route := range routes {
			byRoute[route.ID] = route
		}
		for _, node := range nodes {
			byNode[node.ID] = node
		}
		sources, e := list[Upstream](a.Store, "upstream")
		if e != nil {
			apiError(w, 500, "storage unavailable")
			return
		}
		sourceCounts := map[string]int{}
		for _, source := range sources {
			if source.OwnerAccountID != "" {
				sourceCounts[source.OwnerAccountID]++
			}
		}
		out := []map[string]any{}
		usedByAccount := map[string]int64{}
		for start := 0; start < len(cs); start += 100 {
			end := min(start+100, len(cs))
			conditions := make([]string, 0, end-start)
			args := make([]any, 0, (end-start)*2)
			for _, c := range cs[start:end] {
				cycle, _ := accountCycle(c, time.Now())
				conditions = append(conditions, "(account_id=? AND cycle=?)")
				args = append(args, c.ID, cycle)
			}
			rows, err := a.Store.DB.Query("SELECT account_id,bytes FROM access_usage WHERE "+strings.Join(conditions, " OR "), args...)
			if err != nil {
				apiError(w, 500, "storage unavailable")
				return
			}
			for rows.Next() {
				var accountID string
				var used int64
				if err = rows.Scan(&accountID, &used); err != nil {
					break
				}
				usedByAccount[accountID] = used
			}
			rows.Close()
			if err != nil {
				apiError(w, 500, "storage unavailable")
				return
			}
		}
		for _, c := range cs {
			c.PasswordHash = ""
			c.Credentials = ""
			_, next := accountCycle(c, time.Now())
			used := usedByAccount[c.ID]
			remaining := int64(-1)
			if c.ExpiresAt > 0 {
				remaining = max(int64(0), (c.ExpiresAt-time.Now().Unix()+86399)/86400)
			}
			route, assigned := byRoute[c.RouteID]
			origin, entry := "", ""
			if assigned {
				origin, entry = subscriptionAddressEntry(route, byNode, a.state.Load().snapshot.Version, time.Now().Unix())
			}
			out = append(out, map[string]any{"account": c, "used_bytes": used, "remaining_days": remaining, "next_reset_at": next, "route_alias": route.Alias, "route_origin": origin, "route_entry_label": entry, "user_source_count": sourceCounts[c.ID]})
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
