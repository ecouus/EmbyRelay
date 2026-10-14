# 测试与检查

## 自动化测试

日常开发按修改范围验证；已经准备发布时，由 `bash scripts/publish-server.sh --check`（验证不推送）或 `--publish vX.Y.Z` 统一执行发布检查，避免紧接着重复执行手动完整回归。

```sh
# 前端修改：测试与类型检查/构建
npm --prefix web test
npm --prefix web run build

# 后端修改：运行相关用例；按实际修改替换测试正则
# Go 程序嵌入 web/dist，首次运行前需先完成上面的前端构建
go test -mod=readonly -race ./internal/control -run '^Test(PortalValidationFailurePreservesSource|UserSourcePolicyOnControlAndAgent)$'

# 脚本修改：执行对应脚本测试与语法检查
python3 -m unittest scripts.test_postgres_runner
bash -n scripts/publish-server.sh scripts/test-postgres.sh
```

需要手动完整检查时，在项目根目录运行：

```sh
npm --prefix web ci
npm --prefix web test
npm --prefix web run build
env -u EMBYRELAY_TEST_POSTGRES_URL go test -mod=readonly -race ./...
go vet ./...
python3 -m unittest discover -s scripts -p 'test_*.py'
bash -n scripts/install.sh scripts/install-agent.sh scripts/publish-server.sh scripts/test-postgres.sh
```

完整 SQLite/Go 回归覆盖所有测试；服务器发布和 GitHub CI 的第二阶段仅在真实 PostgreSQL 上运行数据库专项清单，避免再跑整套协议和播放器测试。清单涵盖迁移、复制、约束、持久化、配额、计量、退款和留存，见 [发布指南](publishing.md#开发验证与-postgresql-测试范围)。新增数据库测试时同步维护 `scripts/test-postgres.sh`；清单中的测试缺失会阻止验证通过。

```sh
# 必须先将 EMBYRELAY_TEST_POSTGRES_URL 设置为一次性测试数据库的地址
bash scripts/test-postgres.sh

# 数据库方言、SQL 适配、大范围 schema/存储修改还需完整 PostgreSQL 回归
bash scripts/test-postgres.sh --full
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

## 通用升级回归

```sh
python3 -m unittest scripts.test_upgrade scripts.test_install scripts.test_install_agent
bash -n scripts/upgrade.sh scripts/install.sh scripts/install-agent.sh
shellcheck scripts/upgrade.sh scripts/install.sh scripts/install-agent.sh
# 仅使用临时 Compose 项目、测试数据库与已发布镜像，结束清理测试资源
EMBYRELAY_DOCKER_TEST=1 python3 -m unittest scripts.test_upgrade_docker
```

覆盖真实 1.0.18 → 1.0.21 控制端/Agent 升级、失败回滚、SQLite/PostgreSQL schema、主密钥和真实注册身份保留、停止服务和容器缺失边界、多角色歧义。升级回滚生命周期由公共升级器测试负责；原数据库安装器集成测试保留手动备份恢复及数据库复制/迁移回归。
