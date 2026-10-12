package control

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"embyrelay/internal/proxy"
	"golang.org/x/crypto/bcrypt"
)

func (a *App) accessLogin(ctx context.Context, c AccessAccount, in AccessCommand) (AccessResult, error) {
	if !a.limiter.allow("subscription", in.ClientIP, in.Username) {
		return denied("登录尝试过多，请稍后重试")
	}
	hash := c.PasswordHash
	if !strings.EqualFold(in.Username, c.Username) {
		hash = a.dummyHash
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) != nil || !strings.EqualFold(in.Username, c.Username) {
		a.limiter.fail("subscription", in.ClientIP, in.Username)
		a.limiter.waitFailure(ctx, "subscription", in.Username)
		a.Store.Audit("anonymous", "subscription_login_failed", loginAuditTarget(in.Username, in.ClientIP))
		return denied("用户名或密码错误")
	}
	if err := accountActive(c, time.Now()); err != nil {
		return AccessResult{}, err
	}
	key, err := get[Key](a.Store, "key", in.KeyID)
	if err != nil || key.AccountID != c.ID {
		return denied("访问地址不可用")
	}
	route, err := get[Route](a.Store, "route", key.RouteID)
	if err != nil {
		return denied("路由不可用")
	}
	up, err := get[Upstream](a.Store, "upstream", route.UpstreamID)
	if err != nil || !up.Enabled {
		return denied("源站不可用")
	}
	sealed := ""
	if key.ID == c.KeyID {
		sealed = c.Credentials
	}
	if sealed == "" {
		sealed = up.EncryptedCredentials
	}
	raw, err := a.unseal(sealed)
	if err != nil {
		return denied("源站账号未配置")
	}
	var credentials embyCredentials
	if json.Unmarshal([]byte(raw), &credentials) != nil || credentials.Username == "" || credentials.Password == "" {
		return denied("源站账号未配置")
	}
	identity, policyRaw, err := a.resolveDeviceIdentity(c, up, in)
	if err != nil {
		return denied("设备身份配置不可用")
	}
	target, err := proxy.ValidateURL(up.BaseURL)
	if err != nil {
		return denied("源站地址无效")
	}
	target.Path = strings.TrimRight(target.Path, "/") + "/Users/AuthenticateByName"
	policy := proxy.NetworkPolicy{}
	for _, s := range up.AllowedCIDRs {
		p, e := netip.ParsePrefix(s)
		if e != nil {
			return denied("网络策略无效")
		}
		policy.Allowed = append(policy.Allowed, p)
	}
	transport := &http.Transport{DialContext: policy.DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	payload := Encode(map[string]string{"Username": credentials.Username, "Pw": credentials.Password})
	req, _ := http.NewRequestWithContext(ctx, "POST", target.String(), bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Emby-Authorization", deviceAuthorization(identity.Applied, ""))
	resp, err := client.Do(req)
	if err != nil {
		a.recordDevice(identity, c.ID, up.ID, "源站登录连接失败", 0)
		return denied("源站登录连接失败")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		a.recordDevice(identity, c.ID, up.ID, "源站拒绝登录", resp.StatusCode)
		return denied("源站拒绝登录，请管理员检查源站账号")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return denied("源站登录响应无效")
	}
	var data map[string]any
	if json.Unmarshal(body, &data) != nil {
		return denied("源站登录响应无效")
	}
	sourceToken, _ := data["AccessToken"].(string)
	u, _ := data["User"].(map[string]any)
	uid, _ := u["Id"].(string)
	if sourceToken == "" || uid == "" {
		return denied("源站登录响应缺少 Token 或用户")
	}
	if policy, ok := u["Policy"].(map[string]any); ok && policy["IsAdministrator"] == true {
		return denied("不能绑定源站管理员账号，请使用普通 Emby 账号")
	}
	token := ID()
	encrypted, err := a.seal(sourceToken)
	if err != nil {
		return denied("登录会话保存失败")
	}
	a.accessMu.Lock()
	defer a.accessMu.Unlock()
	tx, err := a.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return denied("订阅服务不可用")
	}
	defer tx.Rollback()
	fresh, err := loadAccount(tx, c.ID)
	if err != nil || fresh.PasswordHash != c.PasswordHash || fresh.Credentials != c.Credentials {
		return denied("账号配置已变化，请重试")
	}
	// A concurrent edit/deletion must not recreate a token for the old source.
	var currentKey, currentRoute, currentUpstream string
	if err = tx.QueryRow("SELECT payload FROM objects WHERE kind='key' AND id=?", key.ID).Scan(&currentKey); err != nil || currentKey != string(Encode(key)) {
		return denied("访问地址已变化，请重试")
	}
	if err = tx.QueryRow("SELECT payload FROM objects WHERE kind='route' AND id=?", route.ID).Scan(&currentRoute); err != nil || currentRoute != string(Encode(route)) {
		return denied("路由已变化，请重试")
	}
	if err = tx.QueryRow("SELECT payload FROM objects WHERE kind='upstream' AND id=?", up.ID).Scan(&currentUpstream); err != nil || currentUpstream != string(Encode(up)) {
		return denied("源站已变化，请重试")
	}
	var currentPolicy string
	policyErr := tx.QueryRow("SELECT payload FROM objects WHERE kind='device_policy' AND id=?", policyObjectID(c.ID, up.ID)).Scan(&currentPolicy)
	if (policyErr != nil && policyErr != sql.ErrNoRows) || currentPolicy != policyRaw {
		return denied("设备身份设置已变化，请重新登录")
	}
	now := time.Now()
	if err = accountActive(fresh, now); err != nil {
		return AccessResult{}, err
	}
	if fresh.StartedAt == 0 {
		fresh.StartedAt = now.Unix()
		if fresh.ExpiresAt == 0 {
			fresh.ExpiresAt = planExpiry(now.Unix(), fresh.Plan)
		}
		if err = saveAccount(tx, fresh); err != nil {
			return denied("订阅激活失败")
		}
	}
	cycle, _ := accountCycle(fresh, now)
	var used int64
	if e := tx.QueryRow("SELECT bytes FROM access_usage WHERE account_id=? AND cycle=?", c.ID, cycle).Scan(&used); e != nil && e != sql.ErrNoRows {
		return denied("流量计费不可用")
	}
	if fresh.Plan.TrafficBytes > 0 && used >= fresh.Plan.TrafficBytes {
		return denied("本周期流量已用尽")
	}
	expires := now.Add(30 * 24 * time.Hour).Unix()
	if fresh.ExpiresAt > 0 && fresh.ExpiresAt < expires {
		expires = fresh.ExpiresAt
	}
	if _, err = tx.Exec("INSERT INTO access_tokens(hash,account_id,source_token,user_id,created_at,expires_at,device,client_ip,user_agent,key_id,device_identity,upstream_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", Hash(token), c.ID, encrypted, uid, now.Unix(), expires, in.Device, in.ClientIP, in.UserAgent, in.KeyID, string(Encode(identity)), up.ID); err != nil {
		return denied("登录会话保存失败")
	}
	if _, err = tx.Exec(`INSERT INTO access_devices(id,account_id,upstream_id,identity,last_seen,last_login,result,source_status) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET identity=excluded.identity,last_seen=excluded.last_seen,last_login=excluded.last_login,result=excluded.result,source_status=excluded.source_status`, identity.RecordID, c.ID, up.ID, string(Encode(identity)), now.Unix(), now.Unix(), "登录成功", 200); err != nil {
		return denied("设备身份保存失败")
	}
	if _, err = tx.Exec("INSERT INTO audit_logs(actor_id,action,target,created_at) VALUES(?,?,?,?)", c.ID, "access_login", c.ID, now.Unix()); err != nil {
		return denied("登录审计失败")
	}
	tx.Exec("DELETE FROM access_tokens WHERE expires_at<=?", now.Unix())
	tx.Exec("DELETE FROM access_targets WHERE expires_at<=?", now.Unix())
	if err = tx.Commit(); err != nil {
		return denied("登录会话保存失败")
	}
	data["AccessToken"] = token
	u["Name"] = c.Username
	if session, ok := data["SessionInfo"].(map[string]any); ok {
		session["UserName"] = c.Username
		if identity.Original.ID != "" {
			session["DeviceId"] = identity.Original.ID
		}
		if identity.Original.Name != "" {
			session["DeviceName"] = identity.Original.Name
		}
		if identity.Original.Client != "" {
			session["Client"] = identity.Original.Client
		}
		if identity.Original.Version != "" {
			session["ApplicationVersion"] = identity.Original.Version
		}
	}
	// Do not expose upstream server connection addresses or additional authentication fields.
	delete(data, "ServerAddress")
	delete(data, "LocalAddress")
	delete(data, "WanAddress")
	return AccessResult{Token: token, UserID: uid, Payload: bytes.ReplaceAll(Encode(data), []byte(sourceToken), []byte(token))}, nil
}
