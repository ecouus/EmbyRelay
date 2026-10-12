# API

管理 API 使用 JSON，错误格式 `{"error":"..."}`。登录后发送 `relay_session` Cookie；写请求添加 `X-Requested-With: EmbyRelay`。所有管理数据响应设置 `Cache-Control: no-store`。operator 只允许读取。

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | /api/v1/auth/login | 统一登录：username, password, code（管理账号 TOTP 可选）；返回角色，subscriber 进入用户中心 |
| GET | /api/v1/auth/me | 当前身份、角色与 TOTP 状态 |
| POST | /api/v1/auth/logout | 注销当前会话 |
| POST | /api/v1/auth/totp | action=setup/enable/disable、password、secret、code |
| GET/POST | /api/v1/upstreams | 列表 / 创建源站 |
| PATCH/DELETE | /api/v1/upstreams/{id} | 部分更新 / 删除（先删除关联路由） |
| GET | /api/v1/upstreams/{id}/health | 受网络策略限制的真实源站检测 |
| GET/POST | /api/v1/routes | 列表 / 创建路由 |
| PATCH/DELETE | /api/v1/routes/{id} | 部分更新 / 删除并撤销该路由密钥 |
| GET/POST | /api/v1/routes/{id}/keys | 密钥列表与历史访问地址 / 生成新密钥；访问地址仅管理员可查看 |
| DELETE | /api/v1/access-keys/{id} | 撤销密钥 |
| GET/POST | /api/v1/users | 仅管理员：列表 / 新建账户 |
| PATCH/DELETE | /api/v1/users/{id} | 重设账户属性/密码并撤销会话 / 删除 |
| GET | /api/v1/relays | 节点、心跳、活跃请求、已应用配置版本 |
| POST | /api/v1/relays/enroll | 生成十分钟一次性注册令牌和服务器公钥 |
| PATCH/DELETE | /api/v1/relays/{id} | enabled 布尔值 / 删除（先取消路由分配） |
| GET | /api/v1/sessions?limit=100 | 本机与节点会话，limit 最大 1000 |
| GET | /api/v1/traffic | 最近最多 1000 条 route/day 流量记录 |
| GET | /api/v1/audit?limit=100 | 审计记录，limit 最大 1000 |
| GET | /api/v1/overview | 控制端运行计数、配置版本与统计 |
| GET | /api/v1/health 或 /healthz | 无认证存活/数据库检查 |
| GET | /metrics | 需管理 Cookie 的 Prometheus 文本指标 |

源站创建：

```json
{"name":"Home","base_url":"http://192.168.1.20:8096","enabled":true,"allowed_cidrs":["192.168.1.20/32"],"redirect_hosts":[],"mode":"proxy"}
```

路由创建：

```json
{"alias":"home","upstream_id":"源站ID","relay_id":"","enabled":true,"rate_bps":0,"daily_quota":0,"max_concurrent":100}
```

`relay_id` 空串表示本机，`auto` 表示自动节点调度，其余是指定节点 ID。rate_bps 为每个节点内该路由共享字节/秒，daily_quota 为所有节点合计 UTC 自然日发送字节；0 不限。创建密钥：`{"expires_at":0}` 表示不过期，其余为未来 Unix 秒。

## Agent 协议

- `POST /api/v1/agent/enroll`：token/name/endpoint/public_key，注册请求本身也需签名。
- `GET /api/v1/agent/config`：返回 base64 payload 和 Ed25519 signature。
- `POST /api/v1/agent/heartbeat`：version/active/traffic/sessions；traffic 使用累计计数，重复上报不重复累加。
- `POST /api/v1/agent/quota`：route_id/date/target；返回 date/granted 累计额度。

签名头：X-Relay-Node、X-Relay-Timestamp、X-Relay-Nonce、X-Relay-Signature。签名内容是 method、RequestURI、timestamp、nonce、body 的 SHA-256 hex，按换行连接。所有协议结构定义见 `internal/control/agents.go`，客户端实现见 `internal/relay/agent.go`。

## 自动中转与导入凭据

安全模式及跳转审批已退役：旧安全设置 GET 返回 false，写操作及跳转审批接口返回 410。快照的 auto_media_redirect 始终为 true，供旧版 Agent 兼容使用。

创建/编辑源站支持可选字段 emby_username、emby_password 和 clear_credentials。省略字段保留已存值，clear_credentials=true 删除账号密码。列表仅返回 has_credentials 标志。

POST /api/v1/upstreams/{id}/credentials：仅管理员，要求登录和 X-Requested-With: EmbyRelay。返回 source_url、username、password；Cache-Control: no-store。只在主动查看导入信息时调用，查看动作记录审计。


## 访问账号与订阅

以下管理接口仅管理员可用，写入沿用管理 API 的会话与 CSRF 校验。

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET / POST | /api/v1/access-plans | 列出 / 创建套餐 |
| PATCH | /api/v1/access-plans/{id} | 提交完整套餐规则；不改变已开通账号 |
| GET / POST | /api/v1/access-accounts | 列出 / 开通账号；创建返回独立 access_key |
| GET | /api/v1/access-accounts/{id}/sessions | 查看最近 100 个登录记录、设备、IP 和 UA |
| POST | /api/v1/access-accounts/{id}/renew | days 或 months 二选一 |
| POST | /api/v1/access-accounts/{id}/status | enabled 布尔值 |
| POST | /api/v1/access-accounts/{id}/password | password 新密码，撤销旧登录 |
| POST | /api/v1/access-accounts/{id}/revoke-sessions | 空对象，撤销全部登录 |
| POST | /api/v1/access-accounts/{id}/reset-usage | 空对象，开启新计量代次，保留旧记录 |

套餐字段：name、duration_days、duration_months、traffic_bytes、traffic_period（total/days/month）、period_days、rate_bps（字节/秒）、max_concurrent（1–1000 个媒体请求）。天数与月数不能同时为正；有效期、流量、速率为 0 表示该项不限。

开通字段：可选 route_id、plan_id、username、password、start_mode（immediate/first_login），可选 expires_at（Unix 秒）、source_username、source_password。源站凭据留空时继承源站配置；两项同时填写时加密保存为此账号专用凭据。列表不返回密码哈希或源站密文。

订阅访问地址下 POST /Users/AuthenticateByName 接收 Username/Pw，返回代理 Token；后续请求携带该 Token。GET /Subscription/Info 返回有效账号的到期时间与流量信息；POST /Sessions/Logout 撤销当前代理 Token。也接受 /emby 前缀。

`POST /api/v1/agent/access` 使用 Ed25519 签名，核对节点、路由、账号和访问密钥归属。

| 项目 | 协议 |
| :--- | :--- |
| 操作 | login / begin / check / charge / finish / logout / wrap / resolve / info |
| 节点能力 | 心跳声明 `subscription_support=true` 和 `device_identity_support=true` |
| 语义拒绝 | HTTP 200 中返回 `error/code`，Agent 检查 `error` |
| 断联 | 拒绝授权 |

## 用户中心与自助源站

站点首页提供统一登录，成功后根据角色进入用户中心或管理控制台。用户中心使用独立 HttpOnly Cookie `relay_portal`，有效期 12 小时，与管理 Cookie 分离。所有写操作要求同源及 `X-Requested-With: EmbyRelay`。

| 方法 | 路径 | 行为 |
| --- | --- | --- |
| POST | /api/v1/portal/login | username/password；使用管理员开通的访问账号 |
| POST | /api/v1/portal/logout | 撤销用户中心当前登录 |
| GET | /api/v1/portal/me | 脱敏账号、共享 used_bytes、next_reset_at |
| GET | /api/v1/portal/sources | 仅本人源站与中转相对地址，不返回源站密码 |
| POST | /api/v1/portal/sources | name/base_url/emby_username/emby_password，创建自有源站、路由及访问密钥 |
| PATCH | /api/v1/portal/sources/{id} | name/base_url 必填；用户名/密码省略或密码留空时保留，撤销该源站旧播放登录 |
| DELETE | /api/v1/portal/sources/{id} | 删除本人源站及关联路由/密钥，保留账号总用量与有效期 |

| 用户中心规则 | 行为 |
| :--- | :--- |
| 自助账号 | 开通时 `route_id` 留空，无需预建源站 |
| 用户名 | 不区分大小写查重；重名账号拒绝用户中心登录 |
| Token 归属 | 绑定访问密钥，不能跨源站串用 |
| 计量与限速 | 按 `account_id` 聚合 |
| 字段权限 | 不接收 owner / quota / relay_id / allowed_cidrs 等管理字段 |
| 到期账号 | 可读取个人信息 |
| 停用、改密、撤销 | 同时撤销用户中心会话 |
| 数据库迁移 | 当前版本 12 |

订阅额度使用双向折算：charge.bytes 仍传实际输出块大小，由控制端统一按 2 倍扣减 access_usage；限速按原始 bytes 计算。旧用量不追溯翻倍，普通路由上下行统计不变。该规则为成本折算，不等于厂商网卡账单。


## 设备身份管理

以下管理端接口要求管理员登录，写操作要求同源和 `X-Requested-With: EmbyRelay`。

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET / PATCH | `/api/v1/upstreams/{id}/device-policy` | 读取 / 设置源站默认规则 |
| GET / PATCH | `/api/v1/access-accounts/{id}/device-policy?upstream_id={source}` | 读取 / 设置已关联源站的账号覆盖 |
| GET | `/api/v1/device-records?upstream_id={source}&account_id={account}` | 按源站 / 账号筛选最近 200 个设备 |
| GET | `/api/v1/portal/devices?upstream_id={source}` | 本人设备记录；忽略调用者传入的 account_id |

GET 策略返回 `policy`、`default_policy` 和 `scope`（source/account/inherit）。PATCH 接收 `{"policy":null}` 可恢复兼容默认或移除账号覆盖；其他更新提交完整 policy。账号必须已通过访问密钥关联所选源站。

```json
{
  "policy": {
    "mode": "grouped",
    "id": "",
    "name": "",
    "client": "",
    "version": "",
    "groups": [
      {"key": "living-room", "id": "room", "name": "客厅设备", "devices": ["original-tv-id", "original-tablet-id"]}
    ],
    "exceptions": [
      {"device": "original-phone-id", "mode": "original"}
    ]
  }
}
```

| 策略字段 | 校验规则 |
| :--- | :--- |
| `mode` | original / stable / custom / grouped |
| `groups.key` | 唯一；1–64 位字母、数字、下划线或短横线 |
| `groups.devices` | 原始设备 ID |
| `exceptions.device` | 原始 ID 精确匹配 |
| 例外 `mode=grouped` | `group` 引用已存在的组 |
| `id/name/client/version` | 最多 128 字节 |
| 设备匹配值 | 最多 512 字节 |
| 字符限制 | 禁止控制字符、引号和反斜杠 |
| `id` | 身份种子；空值仍生成确定身份 |
| `mode=original` | 保留播放器字段，不应用覆盖 |

| 记录字段 | 含义 |
| :--- | :--- |
| `identity.original/applied` | 原始身份 / 发送到源站的身份 |
| `mode/scope/group` | 模式、配置来源、身份组 |
| `last_seen/last_login` | 最近活动 / 登录时间 |
| `result/source_status` | 最近登录结果 / 源站登录响应状态 |
| `last_request_status/last_request_at` | 最近结束请求的状态 / 时间；状态可来自本机或源站 |
| `login_sessions/active_requests` | 登录会话 / 活跃媒体请求数量 |

记录不返回 Token 或密码。身份详情表示最近登录尝试，旧会话可能仍使用此前固定的映射。

| Agent 操作 | 设备身份字段 |
| :--- | :--- |
| `login` | 发送 device_name / client_name / client_version |
| `begin` | 返回 device_identity |
| `finish` | 可发送响应 status |
| 请求重写 | 使用返回身份处理设备头、查询参数和已知播放 JSON 顶层 DeviceId / DeviceName |
| 保留字段 | PlaySessionId / DeviceProfile |
| 第三方媒体域 | 清除身份头 |
| 旧 Agent | access RPC 返回升级错误，不下发访问账号密钥 |


## 中转请求诊断汇总

- `POST /api/v1/agent/diagnostics`：沿用节点签名认证，提交 `records`，每批最多 100 条。控制端返回确认的 `ids`；记录按签名节点归属并去重，不允许请求体指定来源节点。该接口独立于心跳。
- `GET /api/v1/diagnostics?node_id=节点ID`：按节点读取；`node_id=local` 为控制端，不传为全部节点。需要管理端登录。返回节点 ID/名称和脱敏诊断详情，每节点保留 1000 条。
- Agent 本地队列仅在收到确认后标记上传，重启后继续补传。超过本地 1000 条保留上限的历史记录会淘汰。


## 管理员查看用户自助源站

`GET /api/v1/user-sources` 仅允许管理员登录，响应设置 `Cache-Control: no-store`。可传 `account_id` 只读取指定用户的源站。每项包含源站 `id/name/base_url/enabled`、所属用户 `owner_account_id/owner_username/owner_enabled`、`has_credentials`、固定掩码 `source_username_masked/source_password_masked` 及中转 `addresses`。该接口不解密源站凭据，不返回明文、密文或用户密码哈希。

每个中转地址包含 `path/origin/entry_label/available/unavailable_reason`。空 `origin` 表示当前控制端入口；非空时为已就绪的指定节点 origin。复制时组合 origin 与 path，保留用户自己的密钥。已停用、已到期、密钥撤销及无法解密的地址会标记不可用。

用户中心 `GET /api/v1/portal/sources` 保留原有相对 `addresses`，新增 `connection_addresses`，与管理员列表使用相同的入口选择逻辑。该接口仍只返回当前用户自己的源站。
