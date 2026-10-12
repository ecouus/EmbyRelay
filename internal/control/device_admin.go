package control

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

func (a *App) accountUsesSource(account, upstream string) bool {
	var n int
	err := a.Store.DB.QueryRow(`SELECT COUNT(*) FROM objects k JOIN objects r ON r.kind='route' AND r.id=json_extract(k.payload,'$.route_id') WHERE k.kind='key' AND json_extract(k.payload,'$.account_id')=? AND json_extract(r.payload,'$.upstream_id')=?`, account, upstream).Scan(&n)
	return err == nil && n > 0
}
func (a *App) devicePolicyAPI(w http.ResponseWriter, r *http.Request, user User, upstream, account string) {
	if user.Role != "admin" {
		apiError(w, 403, "administrator role required")
		return
	}
	u, err := get[Upstream](a.Store, "upstream", upstream)
	if err != nil {
		apiError(w, 404, "源站不存在")
		return
	}
	if account != "" {
		if _, e := get[AccessAccount](a.Store, "access_account", account); e != nil || !a.accountUsesSource(account, upstream) {
			apiError(w, 404, "账号尚未关联此源站")
			return
		}
	}
	if r.Method == "GET" {
		p := u.DevicePolicy
		scope := "source"
		if account != "" {
			var override DevicePolicy
			var raw string
			e := a.Store.DB.QueryRow("SELECT payload FROM objects WHERE kind='device_policy' AND id=?", policyObjectID(account, upstream)).Scan(&raw)
			if e == nil {
				if json.Unmarshal([]byte(raw), &override) != nil {
					apiError(w, 500, "设备配置读取失败")
					return
				}
				p = &override
				scope = "account"
			} else if e == sql.ErrNoRows {
				p = nil
				scope = "inherit"
			} else {
				apiError(w, 500, "设备配置读取失败")
				return
			}
		}
		writeJSON(w, 200, map[string]any{"policy": p, "default_policy": u.DevicePolicy, "scope": scope})
		return
	}
	if r.Method != "PATCH" {
		apiError(w, 405, "method not allowed")
		return
	}
	var input struct {
		Policy *DevicePolicy `json:"policy"`
	}
	if !decode(w, r, &input) {
		return
	}
	if e := validateDevicePolicy(input.Policy); e != nil {
		apiError(w, 400, e.Error())
		return
	}
	a.mutate(w, func() error {
		a.accessMu.Lock()
		defer a.accessMu.Unlock()
		fresh, e := get[Upstream](a.Store, "upstream", upstream)
		if e != nil {
			return fmt.Errorf("源站不存在")
		}
		if account == "" {
			fresh.DevicePolicy = input.Policy
			return a.Store.put("upstream", upstream, fresh, user.ID)
		}
		if !a.accountUsesSource(account, upstream) {
			return fmt.Errorf("账号尚未关联此源站")
		}
		id := policyObjectID(account, upstream)
		if input.Policy == nil {
			return a.Store.remove("device_policy", id, user.ID)
		}
		return a.Store.put("device_policy", id, input.Policy, user.ID)
	})
}
func (a *App) deviceRecords(w http.ResponseWriter, r *http.Request, account string) {
	if account == "" {
		account = r.URL.Query().Get("account_id")
	}
	upstream := r.URL.Query().Get("upstream_id")
	now := time.Now().Unix()
	rows, err := a.Store.DB.Query(`SELECT d.id,d.account_id,d.upstream_id,d.identity,d.last_seen,d.last_login,d.result,d.source_status,d.last_request_status,d.last_request_at,
 (SELECT COUNT(*) FROM access_tokens t WHERE t.account_id=d.account_id AND t.upstream_id=d.upstream_id AND json_extract(NULLIF(t.device_identity,''),'$.record_id')=d.id AND t.expires_at>?),
 (SELECT COUNT(*) FROM access_leases l JOIN access_tokens t ON t.hash=l.token_hash WHERE t.account_id=d.account_id AND t.upstream_id=d.upstream_id AND json_extract(NULLIF(t.device_identity,''),'$.record_id')=d.id AND t.expires_at>? AND l.is_stream=1 AND l.expires_at>?)
 FROM access_devices d WHERE (?='' OR d.account_id=?) AND (?='' OR d.upstream_id=?)
 ORDER BY d.last_seen DESC LIMIT 200`, now, now, now, account, account, upstream, upstream)
	if err != nil {
		apiError(w, 500, "设备记录读取失败")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, accountID, upstreamID, raw, result string
		var seen, login int64
		var status, requestStatus, sessions, streams int
		var requestAt int64
		if rows.Scan(&id, &accountID, &upstreamID, &raw, &seen, &login, &result, &status, &requestStatus, &requestAt, &sessions, &streams) != nil {
			apiError(w, 500, "设备记录读取失败")
			return
		}
		var identity DeviceIdentity
		if json.Unmarshal([]byte(raw), &identity) != nil {
			apiError(w, 500, "设备记录格式无效")
			return
		}
		out = append(out, map[string]any{"id": id, "account_id": accountID, "upstream_id": upstreamID, "identity": identity, "last_seen": seen, "last_login": login, "result": result, "source_status": status, "last_request_status": requestStatus, "last_request_at": requestAt, "login_sessions": sessions, "active_requests": streams})
	}
	if rows.Err() != nil {
		apiError(w, 500, "设备记录读取失败")
		return
	}
	writeJSON(w, 200, out)
}
