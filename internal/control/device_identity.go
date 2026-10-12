package control

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Device policies apply to account-authenticated playback. Tokens pin their
// resolved identity so an edit does not change an existing upstream session.
type DeviceFields struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Client  string `json:"client"`
	Version string `json:"version"`
}
type DeviceGroup struct {
	DeviceFields
	Key     string   `json:"key"`
	Devices []string `json:"devices"`
}
type DeviceException struct {
	Device string `json:"device"`
	Mode   string `json:"mode"`
	Group  string `json:"group"`
	DeviceFields
}
type DevicePolicy struct {
	Mode string `json:"mode"`
	DeviceFields
	Groups     []DeviceGroup     `json:"groups,omitempty"`
	Exceptions []DeviceException `json:"exceptions,omitempty"`
}
type DeviceIdentity struct {
	RecordID string       `json:"record_id"`
	Original DeviceFields `json:"original"`
	Applied  DeviceFields `json:"applied"`
	Mode     string       `json:"mode"`
	Scope    string       `json:"scope"`
	Group    string       `json:"group,omitempty"`
}

func validDeviceValue(s string, limit int) bool {
	if len(s) > limit {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}
func validateDeviceFields(f DeviceFields) error {
	for _, s := range []string{f.ID, f.Name, f.Client, f.Version} {
		if !validDeviceValue(s, 128) {
			return fmt.Errorf("设备字段最多 128 字节，不能包含引号、反斜杠或控制字符")
		}
	}
	return nil
}
func validateDevicePolicy(p *DevicePolicy) error {
	if p == nil {
		return nil
	}
	if p.Mode != "original" && p.Mode != "stable" && p.Mode != "custom" && p.Mode != "grouped" {
		return fmt.Errorf("设备模式必须为 original、stable、custom 或 grouped")
	}
	if err := validateDeviceFields(p.DeviceFields); err != nil {
		return err
	}
	if len(p.Groups) > 32 || len(p.Exceptions) > 128 {
		return fmt.Errorf("最多配置 32 个身份组和 128 个设备例外")
	}
	groups := map[string]bool{}
	devices := map[string]bool{}
	for _, g := range p.Groups {
		if !aliasPattern.MatchString(g.Key) || groups[g.Key] {
			return fmt.Errorf("身份组标识必须唯一，使用字母、数字、下划线或短横线")
		}
		groups[g.Key] = true
		if err := validateDeviceFields(g.DeviceFields); err != nil {
			return err
		}
		if len(g.Devices) > 128 {
			return fmt.Errorf("每组最多 128 个设备")
		}
		for _, d := range g.Devices {
			if d == "" || !validDeviceValue(d, 512) || devices[d] {
				return fmt.Errorf("设备标识无效或分配到多个身份组")
			}
			devices[d] = true
		}
	}
	exceptions := map[string]bool{}
	for _, e := range p.Exceptions {
		if e.Device == "" || !validDeviceValue(e.Device, 512) || exceptions[e.Device] {
			return fmt.Errorf("设备例外标识无效或重复")
		}
		exceptions[e.Device] = true
		if e.Mode != "original" && e.Mode != "stable" && e.Mode != "custom" && e.Mode != "grouped" {
			return fmt.Errorf("设备例外模式无效")
		}
		if e.Mode == "grouped" && !groups[e.Group] {
			return fmt.Errorf("设备例外引用的身份组不存在")
		}
		if err := validateDeviceFields(e.DeviceFields); err != nil {
			return err
		}
	}
	return nil
}

var deviceAttribute = regexp.MustCompile(`(?i)(?:^|[ ,])(?:DeviceId|Device|Client|Version)\s*=\s*"([^"\r\n]{0,512})"`)

func deviceRequestFields(r *http.Request) DeviceFields {
	f := DeviceFields{}
	for _, h := range []string{"Authorization", "X-MediaBrowser-Authorization", "X-Emby-Authorization"} {
		v := r.Header.Get(h)
		if len(v) > 8192 {
			continue
		}
		for _, m := range deviceAttribute.FindAllStringSubmatch(v, -1) {
			key := strings.ToLower(strings.TrimSpace(strings.TrimLeft(strings.SplitN(m[0], "=", 2)[0], ", ")))
			switch key {
			case "deviceid":
				f.ID = m[1]
			case "device":
				f.Name = m[1]
			case "client":
				f.Client = m[1]
			case "version":
				f.Version = m[1]
			}
		}
	}
	for k, vs := range r.URL.Query() {
		if len(vs) == 0 {
			continue
		}
		v := vs[0]
		switch strings.ToLower(k) {
		case "deviceid", "x-emby-device-id":
			f.ID = v
		case "devicename", "x-emby-device-name":
			f.Name = v
		case "client", "x-emby-client":
			f.Client = v
		case "clientversion", "x-emby-client-version":
			f.Version = v
		}
	}
	for k, p := range map[string]*string{"X-Emby-Device-Id": &f.ID, "X-Emby-Device-Name": &f.Name, "X-Emby-Client": &f.Client, "X-Emby-Client-Version": &f.Version} {
		if v := r.Header.Get(k); v != "" {
			*p = v
		}
	}
	// Header quoting and size validation are repeated on the controller for RPC.
	for _, p := range []*string{&f.ID, &f.Name, &f.Client, &f.Version} {
		if !validDeviceValue(*p, 512) {
			*p = ""
		}
	}
	return f
}
func policyObjectID(account, upstream string) string { return account + ":" + upstream }
func (a *App) resolveDeviceIdentity(c AccessAccount, u Upstream, in AccessCommand) (DeviceIdentity, string, error) {
	original := DeviceFields{ID: in.Device, Name: in.DeviceName, Client: in.ClientName, Version: in.ClientVersion}
	for _, v := range []string{original.ID, original.Name, original.Client, original.Version} {
		if !validDeviceValue(v, 512) {
			return DeviceIdentity{}, "", fmt.Errorf("播放器设备字段无效")
		}
	}
	p := u.DevicePolicy
	scope := "source"
	var override DevicePolicy
	var policyRaw string
	err := a.Store.DB.QueryRow("SELECT payload FROM objects WHERE kind='device_policy' AND id=?", policyObjectID(c.ID, u.ID)).Scan(&policyRaw)
	if err == nil {
		if json.Unmarshal([]byte(policyRaw), &override) != nil {
			return DeviceIdentity{}, "", fmt.Errorf("设备身份配置无效")
		}
		p = &override
		scope = "account"
	} else if err != sql.ErrNoRows {
		return DeviceIdentity{}, "", err
	}
	ns := c.ID + ":" + u.ID + ":"
	identity := DeviceIdentity{RecordID: Hash(ns + original.ID), Original: original, Applied: original, Scope: scope, Mode: "original"}
	if p == nil {
		// Compatibility default: retain the previous Relay login identity.
		identity.Applied = DeviceFields{ID: c.ID + Hash(in.Device)[:16], Name: "Relay", Client: "EmbyRelay", Version: "1.0"}
		identity.Scope = "default"
		return identity, policyRaw, nil
	}
	if err := validateDevicePolicy(p); err != nil {
		return identity, "", err
	}
	mode, fields, group := p.Mode, p.DeviceFields, ""
	for _, e := range p.Exceptions {
		if e.Device == original.ID {
			mode = e.Mode
			fields = mergeDeviceFields(fields, e.DeviceFields)
			group = e.Group
			break
		}
	}
	if mode == "grouped" {
		for _, g := range p.Groups {
			match := group != "" && group == g.Key
			if group == "" {
				for _, d := range g.Devices {
					if d == original.ID {
						match = true
						break
					}
				}
			}
			if match {
				fields = mergeDeviceFields(fields, g.DeviceFields)
				group = g.Key
				break
			}
		}
		if group == "" {
			mode = "stable"
		}
	}
	identity.Mode = mode
	identity.Group = group
	switch mode {
	case "stable":
		identity.Applied.ID = "er-" + Hash(ns + "stable:" + original.ID)[:32]
	case "custom":
		identity.Applied.ID = "er-" + Hash(ns + "custom:" + fields.ID)[:32]
	case "grouped":
		identity.Applied.ID = "er-" + Hash(ns + "group:" + group + ":" + fields.ID)[:32]
	}
	if mode != "original" {
		identity.Applied = mergeDeviceFields(identity.Applied, DeviceFields{Name: fields.Name, Client: fields.Client, Version: fields.Version})
	}
	if identity.Applied.ID == "" {
		identity.Applied.ID = "er-" + Hash(ns + "fallback:" + in.UserAgent)[:32]
	}
	if identity.Applied.Name == "" {
		identity.Applied.Name = "Relay"
	}
	if identity.Applied.Client == "" {
		identity.Applied.Client = "EmbyRelay"
	}
	if identity.Applied.Version == "" {
		identity.Applied.Version = "1.0"
	}
	return identity, policyRaw, nil
}
func mergeDeviceFields(base, over DeviceFields) DeviceFields {
	if over.ID != "" {
		base.ID = over.ID
	}
	if over.Name != "" {
		base.Name = over.Name
	}
	if over.Client != "" {
		base.Client = over.Client
	}
	if over.Version != "" {
		base.Version = over.Version
	}
	return base
}
func deviceAuthorization(f DeviceFields, token string) string {
	value := fmt.Sprintf(`MediaBrowser Client="%s", Device="%s", DeviceId="%s", Version="%s"`, f.Client, f.Name, f.ID, f.Version)
	if token != "" {
		value += fmt.Sprintf(`, Token="%s"`, token)
	}
	return value
}
func identityQueryField(key string) string {
	switch strings.ToLower(key) {
	case "deviceid", "x-emby-device-id":
		return "id"
	case "devicename", "x-emby-device-name":
		return "name"
	case "client", "x-emby-client":
		return "client"
	case "clientversion", "x-emby-client-version":
		return "version"
	}
	return ""
}
func applyDeviceIdentity(r *http.Request, i *DeviceIdentity, token string) error {
	if i == nil {
		return nil
	} // Pre-upgrade tokens remain unchanged until login.
	f := i.Applied
	r.Header.Set("X-Emby-Authorization", deviceAuthorization(f, ""))
	for h, v := range map[string]string{"X-Emby-Device-Id": f.ID, "X-Emby-Device-Name": f.Name, "X-Emby-Client": f.Client, "X-Emby-Client-Version": f.Version} {
		r.Header.Set(h, v)
	}
	q := r.URL.Query()
	for k := range q {
		switch identityQueryField(k) {
		case "id":
			q.Set(k, f.ID)
		case "name":
			q.Set(k, f.Name)
		case "client":
			q.Set(k, f.Client)
		case "version":
			q.Set(k, f.Version)
		}
	}
	r.URL.RawQuery = q.Encode()
	path := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/emby"))
	bodyIdentity := path == "/sessions/playing" || path == "/sessions/playing/progress" || path == "/sessions/playing/stopped" || (path == "/sessions/capabilities" || path == "/sessions/capabilities/full") || strings.HasSuffix(path, "/playbackinfo")
	if !bodyIdentity || r.Body == nil || r.Method != "POST" {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	r.Body.Close()
	if err != nil {
		return fmt.Errorf("设备会话请求读取失败")
	}
	if len(raw) > 1<<20 {
		return fmt.Errorf("设备会话请求超过 1 MiB")
	}
	if len(raw) == 0 {
		r.Body = io.NopCloser(bytes.NewReader(raw))
		return nil
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return fmt.Errorf("设备会话请求必须为 JSON 对象")
	}
	for k := range body {
		switch strings.ToLower(k) {
		case "deviceid":
			body[k] = Encode(f.ID)
		case "devicename":
			body[k] = Encode(f.Name)
		}
	}
	raw = Encode(body)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	r.Header.Del("Content-Length")
	r.GetBody = nil
	return nil
}

func (a *App) recordDevice(i DeviceIdentity, account, upstream, result string, status int) {
	now := time.Now().Unix()
	a.Store.DB.Exec(`INSERT INTO access_devices(id,account_id,upstream_id,identity,last_seen,last_login,result,source_status) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET identity=excluded.identity,last_seen=excluded.last_seen,last_login=excluded.last_login,result=excluded.result,source_status=excluded.source_status`, i.RecordID, account, upstream, string(Encode(i)), now, now, result, status)
}
