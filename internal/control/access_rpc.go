package control

import (
	"context"
	"database/sql"
	"embyrelay/internal/database"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"
)

type AccessCommand struct {
	PublicImage   bool     `json:"public_image,omitempty"`
	Status        int      `json:"status,omitempty"`
	DeviceName    string   `json:"device_name,omitempty"`
	ClientName    string   `json:"client_name,omitempty"`
	ClientVersion string   `json:"client_version,omitempty"`
	Paced         bool     `json:"paced,omitempty"`
	Action        string   `json:"action"`
	AccountID     string   `json:"account_id"`
	KeyID         string   `json:"key_id"`
	Token         string   `json:"token"`
	Lease         string   `json:"lease"`
	Bytes         int64    `json:"bytes"`
	RefundBytes   int64    `json:"refund_bytes,omitempty"`
	Stream        bool     `json:"stream"`
	Username      string   `json:"username"`
	Password      string   `json:"password"`
	Device        string   `json:"device"`
	ClientIP      string   `json:"client_ip"`
	UserAgent     string   `json:"user_agent"`
	Target        string   `json:"target"`
	Targets       []string `json:"targets,omitempty"`
	PaceUnusedNS  int64    `json:"pace_unused_ns,omitempty"`
}
type AccessResult struct {
	DeviceIdentity *DeviceIdentity `json:"device_identity,omitempty"`
	Error          string          `json:"error,omitempty"`
	Code           int             `json:"code,omitempty"`
	Token          string          `json:"token,omitempty"`
	SourceToken    string          `json:"source_token,omitempty"`
	UserID         string          `json:"user_id,omitempty"`
	Lease          string          `json:"lease,omitempty"`
	RateBPS        int64           `json:"rate_bps,omitempty"`
	WaitNS         int64           `json:"wait_ns,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	Target         string          `json:"target,omitempty"`
	Targets        []string        `json:"targets,omitempty"`
}

const streamAccessChunk = 10 << 20
const maxWrapBatch = 1024

// ErrAccessQuota is returned when the billing cycle cannot cover a charge. Agents
// restore it from the RPC error text, which older controls also send unchanged.
var ErrAccessQuota = errors.New("本周期剩余流量不足")

// Smallest shared-rate permit: at most 100 ms and 16 KiB of actual output.
func accessPaceChunk(rate int64) int {
	if rate <= 0 {
		return 16 << 10
	}
	return int(min(int64(16<<10), max(int64(1), rate/10)))
}

// Largest shared-rate permit: about 250 ms of output, never above 1 MiB.
func accessPaceBatch(rate int64) int64 {
	if rate <= 0 {
		return int64(accessPaceChunk(rate))
	}
	return min(int64(1<<20), max(int64(accessPaceChunk(rate)), rate/4))
}

func denied(msg string) (AccessResult, error) { return AccessResult{}, fmt.Errorf("%s", msg) }
func (a *App) accessCall(ctx context.Context, in AccessCommand) (AccessResult, error) {
	if a.AgentMode {
		if a.AccessRPC == nil {
			return denied("节点尚未支持订阅授权")
		}
		out, err := a.AccessRPC(ctx, in)
		if err == nil && out.Error != "" {
			err = fmt.Errorf("%s", out.Error)
			if out.Error == ErrAccessQuota.Error() {
				err = ErrAccessQuota
			}
		}
		return out, err
	}
	return a.accessCommand(ctx, "local", in)
}
func (a *App) accessCommand(ctx context.Context, node string, in AccessCommand) (AccessResult, error) {
	if in.PublicImage && (in.Token != "" || in.Stream || !subscriptionImagePath.MatchString(in.Target) || (in.Action != "begin" && in.Action != "check" && in.Action != "charge" && in.Action != "pace" && in.Action != "finish")) {
		return denied("无效的公开图片授权请求")
	}
	c, err := get[AccessAccount](a.Store, "access_account", in.AccountID)
	if err != nil {
		return denied("访问账号不存在")
	}
	key, err := get[Key](a.Store, "key", in.KeyID)
	if err != nil || key.AccountID != c.ID || !key.Enabled || key.ExpiresAt > 0 && time.Now().Unix() >= key.ExpiresAt {
		return denied("访问地址已失效")
	}
	route, err := get[Route](a.Store, "route", key.RouteID)
	if err != nil || !route.Enabled || (route.OwnerAccountID != "" && route.OwnerAccountID != c.ID) || (node != "local" && route.RelayID != "auto" && route.RelayID != node) {
		return denied("路由不可用")
	}
	up, err := get[Upstream](a.Store, "upstream", route.UpstreamID)
	if err != nil || !up.Enabled {
		return denied("源站不可用")
	}
	if in.Action == "login" {
		return a.accessLogin(ctx, c, in)
	}
	defer a.lockAccount(in.AccountID)()
	if in.Action == "begin" {
		// Cross-account garbage collection stays outside account transactions so
		// concurrent accounts never contend for the same expired rows.
		a.pruneMu.Lock()
		_, err = a.Store.DB.ExecContext(ctx, "DELETE FROM access_leases WHERE expires_at<=?", time.Now().Add(-accessSettlementGrace).Unix())
		a.pruneMu.Unlock()
		if err != nil {
			return denied("会话清理失败")
		}
	}
	tx, err := a.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return denied("订阅服务不可用")
	}
	defer tx.Rollback()
	c, err = loadAccount(tx, in.AccountID)
	if err != nil {
		return denied("访问账号不存在")
	}
	now := time.Now()
	leaseHash := Hash(in.Token)
	if in.PublicImage {
		leaseHash = Hash("public-image:" + in.KeyID)
	}
	if in.Action == "finish" {
		if in.RefundBytes < 0 || in.RefundBytes > streamAccessChunk {
			return denied("invalid refund")
		}
		var chargedCycle string
		var chargedBytes, leaseExpires int64
		err = tx.QueryRow("SELECT charged_cycle,charged_bytes,expires_at FROM access_leases WHERE id=? AND account_id=? AND node_id=? AND token_hash=?", in.Lease, c.ID, node, leaseHash).Scan(&chargedCycle, &chargedBytes, &leaseExpires)
		if err == sql.ErrNoRows {
			return AccessResult{}, tx.Commit()
		}
		if err != nil {
			return denied("订阅服务不可用")
		}
		if in.RefundBytes > 0 && (chargedCycle == "" || in.RefundBytes > chargedBytes) {
			return denied("invalid refund")
		}
		if in.RefundBytes > 0 && leaseExpires <= now.Add(-accessSettlementGrace).Unix() {
			return denied("播放结算已过期")
		}
		result, deleteErr := tx.Exec("DELETE FROM access_leases WHERE id=? AND account_id=? AND node_id=? AND token_hash=?", in.Lease, c.ID, node, leaseHash)
		err = deleteErr
		if err == nil && in.RefundBytes > 0 {
			n, _ := result.RowsAffected()
			if n > 0 {
				_, err = tx.Exec("UPDATE access_usage SET bytes=CASE WHEN bytes>=? THEN bytes-? ELSE 0 END,retired_at=0 WHERE account_id=? AND cycle=?", in.RefundBytes*2, in.RefundBytes*2, c.ID, chargedCycle)
			}
		}
		if err == nil && in.PaceUnusedNS > 0 {
			if n, _ := result.RowsAffected(); n > 0 {
				err = releasePaceSlot(tx, c.ID, now, min(in.PaceUnusedNS, int64(time.Second)))
			}
		}
		if err == nil && in.Status >= 100 && in.Status <= 599 {
			n, _ := result.RowsAffected()
			if n > 0 && !in.PublicImage {
				_, err = tx.Exec(`UPDATE access_devices SET last_request_status=?,last_request_at=? WHERE account_id=? AND id=(SELECT json_extract(NULLIF(device_identity,''),'$.record_id') FROM access_tokens WHERE hash=? AND account_id=? AND key_id=?)`, in.Status, now.Unix(), c.ID, Hash(in.Token), c.ID, in.KeyID)
			}
		}
		if err != nil {
			return denied("订阅服务不可用")
		}
		return AccessResult{}, tx.Commit()
	}
	if err = accountActive(c, now); err != nil {
		return AccessResult{}, err
	}
	var encrypted, userID, identityJSON string
	if in.PublicImage {
		// The URL key is the bearer credential for public thumbnails. A live
		// login for this exact account/key/source is still required. No source
		// token or device session is borrowed to fetch otherwise private images.
		var live int
		err = tx.QueryRow("SELECT COUNT(*) FROM (SELECT 1 FROM access_tokens WHERE account_id=? AND key_id=? AND upstream_id=? AND expires_at>? LIMIT 1) AS live_session", c.ID, in.KeyID, up.ID, now.Unix()).Scan(&live)
		if err != nil || live == 0 {
			return denied("图片访问需要先登录当前订阅入口")
		}
	} else {
		err = tx.QueryRow("SELECT source_token,user_id,device_identity FROM access_tokens WHERE hash=? AND account_id=? AND key_id=? AND expires_at>?", Hash(in.Token), c.ID, in.KeyID, now.Unix()).Scan(&encrypted, &userID, &identityJSON)
		if err != nil {
			return denied("登录已失效，请重新登录")
		}
	}
	// Settle the previous short write in the same transaction before allocating
	// another chunk, preserving its original billing cycle and refund bound.
	if in.Action == "charge" {
		if in.RefundBytes < 0 || in.RefundBytes > streamAccessChunk {
			return denied("invalid refund")
		}
		if in.RefundBytes > 0 {
			var chargedCycle string
			var chargedBytes, expires int64
			err = tx.QueryRow("SELECT charged_cycle,charged_bytes,expires_at FROM access_leases WHERE id=? AND account_id=? AND node_id=? AND token_hash=?", in.Lease, c.ID, node, leaseHash).Scan(&chargedCycle, &chargedBytes, &expires)
			if err != nil || chargedCycle == "" || in.RefundBytes > chargedBytes || expires <= now.Unix() {
				return denied("invalid refund")
			}
			if _, err = tx.Exec("UPDATE access_usage SET bytes=CASE WHEN bytes>=? THEN bytes-? ELSE 0 END,retired_at=0 WHERE account_id=? AND cycle=?", in.RefundBytes*2, in.RefundBytes*2, c.ID, chargedCycle); err != nil {
				return denied("订阅服务不可用")
			}
		}
	}
	cycle, _ := accountCycle(c, now)
	var used int64
	if e := tx.QueryRow("SELECT bytes FROM access_usage WHERE account_id=? AND cycle=?", c.ID, cycle).Scan(&used); e != nil && e != sql.ErrNoRows {
		return denied("流量计费不可用")
	}
	if in.Action != "info" && in.Action != "logout" && in.Action != "check" && in.Action != "pace" && c.Plan.TrafficBytes > 0 && used >= c.Plan.TrafficBytes {
		return denied("本周期流量已用尽")
	}
	out := AccessResult{UserID: userID}
	if identityJSON != "" {
		var identity DeviceIdentity
		if json.Unmarshal([]byte(identityJSON), &identity) != nil {
			return denied("设备身份记录无效，请重新登录")
		}
		out.DeviceIdentity = &identity
	}
	switch in.Action {
	case "info":
		_, next := accountCycle(c, now)
		remaining := int64(-1)
		if c.Plan.TrafficBytes > 0 {
			remaining = max(int64(0), c.Plan.TrafficBytes-used)
		}
		out.Payload = Encode(map[string]any{"username": c.Username, "expires_at": c.ExpiresAt, "used_bytes": used, "remaining_bytes": remaining, "next_reset_at": next})
	case "begin":
		if out.DeviceIdentity != nil {
			if _, err = tx.Exec("UPDATE access_devices SET last_seen=? WHERE id=?", now.Unix(), out.DeviceIdentity.RecordID); err != nil {
				return denied("设备状态更新失败")
			}
		}
		var n int
		if in.Stream {
			if err = tx.QueryRow("SELECT COUNT(*) FROM access_leases WHERE account_id=? AND is_stream=1 AND expires_at>?", c.ID, now.Unix()).Scan(&n); err != nil {
				return denied("并发检查失败")
			}
			if n >= c.Plan.MaxConcurrent {
				return denied("同时播放数已达上限")
			}
		}
		out.Lease = ID()
		isStream := 0
		if in.Stream {
			isStream = 1
		}
		paced := 0
		if in.Paced {
			paced = 1
			out.RateBPS = c.Plan.RateBPS
		}
		if _, err = tx.Exec("INSERT INTO access_leases(id,account_id,token_hash,node_id,expires_at,is_stream,paced) VALUES(?,?,?,?,?,?,?)", out.Lease, c.ID, leaseHash, node, now.Unix()+30, isStream, paced); err != nil {
			return denied("会话创建失败")
		}
		if !in.PublicImage {
			out.SourceToken, err = a.unseal(encrypted)
			if err != nil {
				return denied("源站凭据不可用")
			}
		}
	case "check", "charge", "pace":
		var paced int
		if err = tx.QueryRow("SELECT paced FROM access_leases WHERE id=? AND account_id=? AND token_hash=? AND node_id=? AND expires_at>?", in.Lease, c.ID, leaseHash, node, now.Unix()).Scan(&paced); err != nil {
			return denied("播放授权已失效")
		}
		if _, err = tx.Exec("UPDATE access_leases SET expires_at=? WHERE id=?", now.Unix()+30, in.Lease); err != nil {
			return denied("授权续期失败")
		}
		if in.Action == "pace" {
			if paced != 1 || in.Bytes <= 0 || in.Bytes > accessPaceBatch(c.Plan.RateBPS) {
				return denied("invalid pace request")
			}
			if c.Plan.RateBPS > 0 {
				var next int64
				err = tx.QueryRow("SELECT next_ns FROM access_rate WHERE account_id=?", c.ID).Scan(&next)
				if err != nil && err != sql.ErrNoRows {
					return denied("限速服务不可用")
				}
				if next > now.UnixNano() {
					// Waiting does not reserve future credit. Cancellation and
					// concurrent waiters cannot create an unspent queue of debt.
					out.WaitNS = next - now.UnixNano()
				} else {
					duration := int64(float64(in.Bytes) / float64(c.Plan.RateBPS) * float64(time.Second))
					if _, err = tx.Exec("INSERT INTO access_rate VALUES(?,?) ON CONFLICT(account_id) DO UPDATE SET next_ns=excluded.next_ns", c.ID, now.UnixNano()+duration); err != nil {
						return denied("限速服务不可用")
					}
				}
			}
		}
		if in.Action == "charge" {
			if in.Bytes < 0 || in.Bytes > streamAccessChunk {
				return denied("invalid charge")
			}
			// Subscription billing estimates source ingress plus client egress.
			// Apply centrally so local and Agent traffic use the same multiplier.
			billedBytes := in.Bytes * 2
			if c.Plan.TrafficBytes > 0 && billedBytes > c.Plan.TrafficBytes-used {
				return AccessResult{}, ErrAccessQuota
			}
			if _, err = tx.Exec("INSERT INTO access_usage(account_id,cycle,bytes) VALUES(?,?,?) ON CONFLICT(account_id,cycle) DO UPDATE SET bytes=access_usage.bytes+excluded.bytes,retired_at=0", c.ID, cycle, billedBytes); err != nil {
				return denied("流量计费失败")
			}
			if _, err = tx.Exec("UPDATE access_leases SET charged_cycle=?,charged_bytes=? WHERE id=?", cycle, in.Bytes, in.Lease); err != nil {
				return denied("流量计费失败")
			}
			if c.Plan.RateBPS > 0 && paced == 0 {
				var next int64
				tx.QueryRow("SELECT next_ns FROM access_rate WHERE account_id=?", c.ID).Scan(&next)
				next = max(next, now.UnixNano()) + int64(float64(in.Bytes)/float64(c.Plan.RateBPS)*float64(time.Second))
				out.WaitNS = next - now.UnixNano()
				if _, err = tx.Exec("INSERT INTO access_rate VALUES(?,?) ON CONFLICT(account_id) DO UPDATE SET next_ns=excluded.next_ns", c.ID, next); err != nil {
					return denied("限速服务不可用")
				}
			}
		}
	case "logout":
		if _, err = tx.Exec("DELETE FROM access_tokens WHERE hash=? AND account_id=?", Hash(in.Token), c.ID); err != nil {
			return denied("退出失败")
		}
	case "wrap":
		u, e := url.Parse(in.Target)
		if e != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || len(in.Target) > 16384 {
			return denied("媒体地址无效")
		}
		sealed, e := a.seal(in.Target)
		if e != nil {
			return denied("媒体地址不可用")
		}
		id := ID()
		if _, err = tx.Exec("INSERT INTO access_targets VALUES(?,?,?,?,?)", id, c.ID, Hash(in.Token), sealed, now.Add(24*time.Hour).Unix()); err != nil {
			return denied("媒体地址保存失败")
		}
		out.Target = id
	case "wrap_batch":
		if len(in.Targets) == 0 || len(in.Targets) > maxWrapBatch {
			return denied("媒体地址无效")
		}
		out.Targets = make([]string, len(in.Targets))
		for i, target := range in.Targets {
			sealed, e := a.sealMediaTarget(target)
			if e != nil {
				return denied(e.Error())
			}
			out.Targets[i] = ID()
			if _, err = tx.Exec("INSERT INTO access_targets VALUES(?,?,?,?,?)", out.Targets[i], c.ID, Hash(in.Token), sealed, now.Add(24*time.Hour).Unix()); err != nil {
				return denied("媒体地址保存失败")
			}
		}
	case "resolve":
		var sealed string
		if err = tx.QueryRow("SELECT target FROM access_targets WHERE id=? AND account_id=? AND token_hash=? AND expires_at>?", in.Target, c.ID, Hash(in.Token), now.Unix()).Scan(&sealed); err != nil {
			return denied("媒体地址已失效，请重新播放")
		}
		out.Target, err = a.unseal(sealed)
		if err != nil {
			return denied("媒体地址不可用")
		}
	default:
		return denied("invalid subscription action")
	}
	if err = tx.Commit(); err != nil {
		return denied("订阅服务不可用")
	}
	return out, nil
}

// Validate and encrypt a media target; the error text is safe to show.
func (a *App) sealMediaTarget(target string) (string, error) {
	u, e := url.Parse(target)
	if e != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || len(target) > 16384 {
		return "", errors.New("媒体地址无效")
	}
	sealed, e := a.seal(target)
	if e != nil {
		return "", errors.New("媒体地址不可用")
	}
	return sealed, nil
}

// Give back the unused tail of a request's last shared-rate permit. Permits are
// only granted once the previous slot ended, so a booking no longer than the
// reported tail belongs to this request; a longer one means another stream
// booked after it and must stay.
func releasePaceSlot(tx *database.Tx, account string, now time.Time, unused int64) error {
	var next int64
	if err := tx.QueryRow("SELECT next_ns FROM access_rate WHERE account_id=?", account).Scan(&next); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if remaining := next - now.UnixNano(); remaining > 0 && remaining <= unused+int64(20*time.Millisecond) {
		_, err := tx.Exec("UPDATE access_rate SET next_ns=? WHERE account_id=?", now.UnixNano(), account)
		return err
	}
	return nil
}
