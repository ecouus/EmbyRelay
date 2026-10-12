package control

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

type UserSourceAddress struct {
	RouteID           string `json:"route_id"`
	RouteAlias        string `json:"route_alias"`
	Path              string `json:"path"`
	Origin            string `json:"origin"`
	NodeID            string `json:"node_id"`
	EntryLabel        string `json:"entry_label"`
	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

type UserSourceSummary struct {
	ID                   string              `json:"id"`
	Name                 string              `json:"name"`
	BaseURL              string              `json:"base_url"`
	Enabled              bool                `json:"enabled"`
	OwnerAccountID       string              `json:"owner_account_id"`
	OwnerUsername        string              `json:"owner_username"`
	OwnerEnabled         bool                `json:"owner_enabled"`
	HasCredentials       bool                `json:"has_credentials"`
	SourceUsernameMasked string              `json:"source_username_masked"`
	SourcePasswordMasked string              `json:"source_password_masked"`
	Addresses            []UserSourceAddress `json:"addresses"`
}

func (a *App) userSources(w http.ResponseWriter, r *http.Request, user User) {
	w.Header().Set("Cache-Control", "no-store")
	if user.Role != "admin" {
		apiError(w, 403, "administrator role required")
		return
	}
	if r.Method != "GET" {
		apiError(w, 405, "method not allowed")
		return
	}
	sources, err := list[Upstream](a.Store, "upstream")
	if err != nil {
		apiError(w, 500, "sources unavailable")
		return
	}
	accounts, err := list[AccessAccount](a.Store, "access_account")
	if err != nil {
		apiError(w, 500, "accounts unavailable")
		return
	}
	routes, err := list[Route](a.Store, "route")
	if err != nil {
		apiError(w, 500, "routes unavailable")
		return
	}
	keys, err := list[Key](a.Store, "key")
	if err != nil {
		apiError(w, 500, "keys unavailable")
		return
	}
	nodes, err := list[Node](a.Store, "node")
	if err != nil {
		apiError(w, 500, "nodes unavailable")
		return
	}
	owners := map[string]AccessAccount{}
	for _, c := range accounts {
		owners[c.ID] = c
	}
	bySource := map[string][]Route{}
	for _, route := range routes {
		bySource[route.UpstreamID] = append(bySource[route.UpstreamID], route)
	}
	byRoute := map[string][]Key{}
	for _, key := range keys {
		byRoute[key.RouteID] = append(byRoute[key.RouteID], key)
	}
	byNode := map[string]Node{}
	for _, node := range nodes {
		byNode[node.ID] = node
	}
	filter := r.URL.Query().Get("account_id")
	now := time.Now().Unix()
	configVersion := a.state.Load().snapshot.Version
	out := []UserSourceSummary{}
	for _, source := range sources {
		if source.OwnerAccountID == "" || (filter != "" && source.OwnerAccountID != filter) {
			continue
		}
		owner, ownerExists := owners[source.OwnerAccountID]
		item := UserSourceSummary{ID: source.ID, Name: source.Name, BaseURL: source.BaseURL, Enabled: source.Enabled, OwnerAccountID: source.OwnerAccountID, OwnerUsername: owner.Username, OwnerEnabled: owner.Enabled, HasCredentials: source.EncryptedCredentials != "", Addresses: []UserSourceAddress{}}
		if !ownerExists {
			item.OwnerUsername = "账号已删除"
		}
		// No credential decryption is needed for this projection; neither the
		// ciphertext nor the source username/password is included in the response.
		if item.HasCredentials {
			item.SourceUsernameMasked = "••••••••"
			item.SourcePasswordMasked = "••••••••"
		}
		for _, route := range bySource[source.ID] {
			if route.OwnerAccountID != source.OwnerAccountID {
				continue
			}
			for _, key := range byRoute[route.ID] {
				if key.AccountID != source.OwnerAccountID {
					continue
				}
				addr := UserSourceAddress{RouteID: route.ID, RouteAlias: route.Alias, NodeID: route.RelayID, EntryLabel: "控制服务器", Available: true}
				switch {
				case !ownerExists || !owner.Enabled:
					addr.UnavailableReason = "用户账号不可用"
				case owner.ExpiresAt > 0 && owner.ExpiresAt <= now:
					addr.UnavailableReason = "订阅已到期"
				case !source.Enabled:
					addr.UnavailableReason = "源站已停用"
				case !route.Enabled:
					addr.UnavailableReason = "路由已停用"
				case !key.Enabled:
					addr.UnavailableReason = "访问密钥已撤销"
				case key.ExpiresAt > 0 && key.ExpiresAt <= now:
					addr.UnavailableReason = "访问密钥已到期"
				}
				addr.Origin, addr.EntryLabel = subscriptionAddressEntry(route, byNode, configVersion, now)
				if key.EncryptedSecret != "" {
					secret, err := a.unseal(key.EncryptedSecret)
					if err == nil && secret != "" {
						addr.Path = "/r/" + route.Alias + "/" + secret + "/"
					} else {
						addr.UnavailableReason = "访问地址无法解密"
					}
				} else {
					addr.UnavailableReason = "未保存访问地址"
				}
				addr.Available = addr.UnavailableReason == "" && addr.Path != ""
				item.Addresses = append(item.Addresses, addr)
			}
		}
		sort.Slice(item.Addresses, func(i, j int) bool {
			return item.Addresses[i].RouteAlias+item.Addresses[i].Path < item.Addresses[j].RouteAlias+item.Addresses[j].Path
		})
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		left := strings.ToLower(out[i].OwnerUsername + "\x00" + out[i].Name)
		right := strings.ToLower(out[j].OwnerUsername + "\x00" + out[j].Name)
		if left == right {
			return out[i].ID < out[j].ID
		}
		return left < right
	})
	writeJSON(w, 200, out)
}

// Admin and portal views use the same entry selection as subscription routing.
// Automatic routing enters through the control server so nodes can be selected
// at request time. A fixed node is offered only after it is ready for this config.
func subscriptionAddressEntry(route Route, nodes map[string]Node, configVersion, now int64) (string, string) {
	if route.RelayID == "auto" {
		return "", "自动选择在线节点（控制端入口）"
	}
	if route.RelayID == "" {
		return "", "控制服务器"
	}
	node, ok := nodes[route.RelayID]
	if ok && node.Enabled && node.SubscriptionSupport && node.DeviceIdentitySupport && node.ConfigVersion >= configVersion && now-node.LastSeen < 60 {
		if origin := diagnosticOrigin(node.Endpoint); origin != "" {
			return origin, node.Name
		}
	}
	return "", "指定节点未就绪（控制端入口）"
}
