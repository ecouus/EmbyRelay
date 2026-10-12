# 测试与检查

## 自动化测试

在项目根目录运行：

```sh
go test -race ./...
go vet ./...
npm --prefix web ci
npm --prefix web run build
python3 -m unittest discover -s scripts -p 'test_*.py'
bash -n scripts/install.sh scripts/install-agent.sh
```

Go 测试使用本机 HTTP 测试服务与临时数据库。前端构建包含 Vue TypeScript 类型检查。

| 范围 | 测试内容 |
| :--- | :--- |
| 管理与登录 | 管理员与只读权限、统一登录、CSRF、TOTP、密码与会话撤销 |
| 源站与路由 | 配置持久化、地址补全、密钥过期和撤销、跨用户数据隔离 |
| 媒体转发 | GET / POST / WebSocket、Range、ETag、读取超时恢复、跳转与认证头处理 |
| 流量与额度 | 计量、共享限速、并发控制、配额预留、订阅周期与双向折算 |
| 中转节点 | 注册、签名、防重放、配置缓存、流量上报与签名访问 RPC |
| 用户中心 | 自助源站、共享账号额度、到期查看、源站删除与登录撤销 |
| 设备身份 | 模式、身份分组、设备例外、账号与源站隔离、请求重写、独立播放会话 |
| 数据迁移 | 旧版本升级、现有 Token 保留与设备记录结构 |
| 安装脚本 | Nginx 配置识别、备份与替换、安装与升级配置处理 |

## 本机集成检查

`tests/integration/smoke.py` 提供管理登录、源站与路由创建、并发 Range 请求、流量核对和测试数据清理。`cmd/faultupstream` 提供合成媒体与故障响应：

```sh
go run ./cmd/faultupstream
```

## 部署检查

部署后通过 `/healthz` 检查服务和数据库；在控制台检测源站连通性。使用播放器登录中转地址，检查播放、拖动、暂停、恢复、字幕与转码。播放会话可查看客户端、节点和流量；请求诊断可查看失败原因与回源字段。

设备身份设置保存后，通过新登录检查原始设备与上游身份映射；已有登录沿用创建时的身份。

`tests/load/playback.js` 提供 k6 媒体请求脚本：

```sh
RELAY_URL='完整带密钥的媒体流地址' VUS=100 DURATION=10m k6 run tests/load/playback.js
```

脚本使用固定请求名称并关闭 URL 系统标签，避免将访问密钥写入统计标签。测试结果取决于媒体源、服务器与网络配置。
