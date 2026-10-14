package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"embyrelay/internal/proxy"
)

// verifyEmbySource verifies protocol behavior, not the identity of an untrusted server.
func verifyEmbySource(ctx context.Context, u Upstream, credentials embyCredentials) error {
	policy := proxy.NetworkPolicy{}
	for _, cidr := range u.AllowedCIDRs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return fmt.Errorf("源站网络策略无效")
		}
		policy.Allowed = append(policy.Allowed, prefix)
	}
	transport := &http.Transport{DialContext: policy.DialContext, TLSHandshakeTimeout: 4 * time.Second, ResponseHeaderTimeout: 4 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	base, err := proxy.ValidateURL(u.BaseURL)
	if err != nil {
		return fmt.Errorf("源站地址无效")
	}
	identity := DeviceFields{ID: "embyrelay-verify-" + ID(), Name: "EmbyRelay source validation", Client: "EmbyRelay", Version: "1"}
	request := func(method, path, token string, payload any, result any) error {
		target := *base
		target.Path = strings.TrimRight(base.Path, "/") + path
		target.RawPath = ""
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(Encode(payload))
		}
		req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
		if err != nil {
			return fmt.Errorf("Emby 验证请求无效")
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Emby-Authorization", deviceAuthorization(identity, token))
		if token != "" {
			req.Header.Set("X-Emby-Token", token)
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("无法连接 Emby 源站，请检查公网地址、证书和服务状态")
		}
		defer resp.Body.Close()
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return fmt.Errorf("Emby 账号验证失败，请检查用户名、密码和账号权限")
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("源站未通过 Emby 验证（HTTP %d）", resp.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		if err != nil || len(data) > 1<<20 || json.Unmarshal(data, result) != nil {
			return fmt.Errorf("源站返回无效的 Emby 响应")
		}
		return nil
	}
	var info struct {
		ID         string `json:"Id"`
		Version    string
		ServerName string
	}
	if err := request("GET", "/System/Info/Public", "", nil, &info); err != nil {
		return err
	}
	if info.ID == "" || info.Version == "" || info.ServerName == "" {
		return fmt.Errorf("源站缺少 Emby 服务器信息")
	}
	var login struct {
		AccessToken string
		ServerID    string `json:"ServerId"`
		User        struct {
			ID     string `json:"Id"`
			Policy struct{ IsAdministrator bool }
		}
	}
	if err := request("POST", "/Users/AuthenticateByName", "", map[string]string{"Username": credentials.Username, "Pw": credentials.Password}, &login); err != nil {
		return err
	}
	// Always revoke the temporary validation session, including failed token checks.
	defer func() {
		if login.AccessToken == "" {
			return
		}
		cleanup, end := context.WithTimeout(context.Background(), 2*time.Second)
		defer end()
		target := *base
		target.Path = strings.TrimRight(base.Path, "/") + "/Sessions/Logout"
		target.RawPath = ""
		req, _ := http.NewRequestWithContext(cleanup, "POST", target.String(), nil)
		req.Header.Set("X-Emby-Token", login.AccessToken)
		req.Header.Set("X-Emby-Authorization", deviceAuthorization(identity, login.AccessToken))
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	if login.AccessToken == "" || login.User.ID == "" || (login.ServerID != "" && login.ServerID != info.ID) {
		return fmt.Errorf("源站登录响应缺少有效的 Emby 会话或服务器信息不一致")
	}
	if login.User.Policy.IsAdministrator {
		return fmt.Errorf("不能绑定源站管理员账号，请使用普通 Emby 账号")
	}

	var user struct {
		ID       string `json:"Id"`
		ServerID string `json:"ServerId"`
		Policy   struct{ IsAdministrator bool }
	}
	if err := request("GET", "/Users/Me", login.AccessToken, nil, &user); err != nil {
		return err
	}
	if user.ID != login.User.ID || (user.ServerID != "" && user.ServerID != info.ID) {
		return fmt.Errorf("Emby Token 验证失败或用户信息不一致")
	}
	if user.Policy.IsAdministrator {
		return fmt.Errorf("不能绑定源站管理员账号，请使用普通 Emby 账号")
	}
	return nil
}
