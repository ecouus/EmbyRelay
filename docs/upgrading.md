# 通用一键升级

控制端与 Agent 使用同一入口。无需 SSH 登录其他主机，也无需事先知道角色、容器名或镜像标签。每台主机分别执行；脚本只操作选中的本机实例。

```bash
curl --proto '=https' --tlsv1.2 -fsSL --retry 3 https://raw.githubusercontent.com/ecouus/EmbyRelay/main/scripts/upgrade.sh -o embyrelay-upgrade.sh && sudo bash embyrelay-upgrade.sh
```

仓库检出环境可直接执行 `sudo bash scripts/upgrade.sh`。单文件下载入口通过 HTTPS 获取同一仓库的公共 Python 实现；查询或下载失败会退出，不会报告“已经最新”。该入口不依赖 GitHub Release。

## 范围与运行要求

- 本机 Linux、root 权限、Docker Engine、Compose 插件 2.20+、Python 3.9+、curl、可信 CA；不支持远程 Docker daemon、Docker Desktop、Swarm、裸二进制或单独 `docker run` 部署。
- JSON 配置无需额外解析库；YAML 使用 PyYAML 的安全解析器，需 `python3-yaml`。Debian/Ubuntu 可执行 `sudo apt-get install python3 python3-yaml curl ca-certificates`。不加载 YAML 对象、不执行 `.env`、不 `source` 配置。
- 已验证环境：Debian 13 / x86_64、Docker Engine 29.9、Compose 5.6、Python 3.13、PostgreSQL 17。其他 Linux/工具版本和 ARM64 尚未做真实生命周期回归，不声称完成验证；架构不匹配的镜像会被拒绝。

| 部署方式 | 支持范围 |
| --- | --- |
| 两套安装脚本生成的 JSON Compose | 支持；保留原角色、项目、网络、反代与挂载，不重新配置 Nginx 或申请证书 |
| README / deploy 下的手动 Compose | 支持安全 YAML/JSON、bridge 或 host 网络；应用使用官方镜像、标准可执行文件及唯一持久数据目录 |
| 自定义目录、项目名、服务名 | 支持；依据实际 Compose 标签和数据挂载确认，不按目录名称猜测角色 |
| 多文件 Compose、`.env`、`env_file`、文件 secrets | 支持；保留原文件顺序及插值，备份配置和文件凭据；改写最后一份 Compose 中目标服务的镜像字段 |
| SQLite / Agent | 停服后完整归档实际数据挂载，包括数据库、WAL/SHM、`master.key`、`identity.json`；恢复时完整替换 |
| PostgreSQL 控制端 | 仅同一 Compose 中专用 PostgreSQL 17、标准数据库/用户名、public schema；停应用后 `pg_dump -Fc`，失败时单事务重建并恢复 public schema，与本地密钥一起恢复 |
| 外部/共享数据库、额外可写挂载、数据子挂载 | 安全退出，需管理员先设计匹配的备份恢复流程 |
| `include` / `extends`、自定义 entrypoint/command、多副本、原生 TLS | 暂不支持；转换为明确的多文件配置或人工升级，脚本不猜测 |

PostgreSQL 数据库容器保持运行，其镜像、大版本、卷、账号与密码不变；脚本只停止/重建应用服务。备份前拒绝其他数据库连接及其他用户 schema。应用数据卷有其他运行容器写入时拒绝操作。无法判断的自定义部署会在修改数据前报错。

## 发现与选择

默认检查当前目录、原安装目录入口，以及所有运行/已停止容器的 Compose 项目标签。原安装目录仅用于查找，仍须确认实际 Compose 与容器；不会递归扫描整台主机。容器已删除的自定义目录须显式提供。

```bash
# 控制端和 Agent 命令相同；路径均为用户自己的部署目录
sudo bash embyrelay-upgrade.sh --directory /srv/media-relay
sudo bash embyrelay-upgrade.sh --directory /srv/media-node --version 1.0.21

# 多实例或混合角色：交互时显示列表；无人值守必须明确选择
sudo bash embyrelay-upgrade.sh --instance media-project/node --yes
sudo bash embyrelay-upgrade.sh --directory /srv/media-relay --role control --yes

# 多份 Compose 文件按部署时的顺序指定
sudo bash embyrelay-upgrade.sh --file /srv/media-relay/compose.yml --file /srv/media-relay/production.yml --instance media-project/panel --yes
```

`--yes`（别名 `--non-interactive`）仅省略升级确认，不会在多实例时选择第一个。容器与配置出现数据库/身份或挂载漂移时，先处理漂移；脚本不把未知卷当成空卷初始化。

已停止服务可升级，成功后启动新服务，失败回滚保留原来的停止状态。容器缺失时须有此升级器已完成事务的镜像记录，或明确提供之前捕获的 `--old-image sha256:...`，且数据仍存在。无镜像证据、无数据库、无密钥或无 Agent 身份时拒绝初始化。旧镜像必须还在本机，禁止清理它后再尝试回滚。

## 版本与执行过程

默认分页查询 Docker Hub 所有版本标签，按语义版本选择最高正式版，不依赖 GitHub Release；不直接使用可能变动的 latest。同版本跳过，`--force` 才备份并重建。

```bash
sudo bash embyrelay-upgrade.sh --version 1.0.21 --yes
sudo bash embyrelay-upgrade.sh --version 1.1.0-rc.1 --prerelease --yes
# 不指定版本时 --prerelease 允许发现预发布
sudo bash embyrelay-upgrade.sh --version 1.0.20 --allow-downgrade --yes
```

降级还要求实际数据库 schema 不高于目标镜像支持的 schema；不自动反向迁移数据库。版本列表、镜像拉取、鉴权、架构或隔离启动失败均会明确退出。

升级过程：

1. 发现并锁定实例，展示角色、目录、当前与目标版本；原安装入口与公共升级器共享目录锁。
2. 拉取目标版本、检查架构，在无网络、无生产挂载的临时环境读取实际软件版本及 schema。
3. 记录实际旧镜像 ID/digest、原配置、原运行状态；检查数据独占与空间。
4. 停止选中的应用，完整备份数据与配置。PostgreSQL 额外导出数据库，备份目录 0700、文件 0600。
5. 可靠解析 Compose 并更新目标服务的固定版本；仅重建该服务，禁用构建、拉取和依赖服务重建。
6. 检查本机健康、角色、实际版本、数据库类型/schema、运行镜像 ID、主密钥和 Agent 身份。
7. 验证已确认的 HTTPS origin；成功保留备份，失败恢复旧配置和完整数据，用捕获的本地旧镜像重建并再次验收。

升级器使用备份目录中的私有镜像覆盖文件，固定实际镜像 ID 对应的本地标签，避免旧标签变化；后续发现会识别该覆盖文件并回到原始 Compose 文件列表。普通配置的目标服务镜像同时更新为具体版本，避免日后仍拉取原固定标签。保留备份和本地旧镜像，不自动清理构建缓存或历史备份。

## HTTPS 与 Agent 验收

安装元数据中的域名、应用 `RELAY_PUBLIC_URL` 或 `io.embyrelay.public-url` 标签用于确定 HTTPS 入口。外部手动反代无法从 Compose 明确确定域名时，指定自己的 origin：

```bash
sudo bash embyrelay-upgrade.sh --directory /srv/media-relay --public-url https://relay.example.com --yes
```

不跟随重定向，不忽略证书错误；HTTPS 响应还需匹配版本、角色与数据库类型。未配置明确入口时报告本机验收成功、公网入口待用户验证，不声称 HTTPS 已验证。暂不支持入口子路径或要求额外 HTTP 认证的反代自动验收。

Agent 的本机 `/healthz` 不能证明控制端已经收到心跳或配置同步完成。升级后必须在控制端确认：节点在线、配置版本同步、身份未变、用量无异常。自动回滚可能恢复较早的 Agent 累计计量；若控制端报告 `counters cannot decrease`，停止分配流量并按节点恢复流程处理，不能假称整个链路已健康。

## 失败、中断与人工恢复

备份保存在实际部署目录的 `backups/upgrade-VERSION-TIMESTAMP/`；事务阶段位于 `.upgrade-SERVICE.json`。信号中断会尝试恢复，恢复期间忽略再次中断。SIGKILL、断电无法自动收尾，下次执行会拒绝覆盖未完成事务并指向保留的备份。

```bash
# 使用自己的备份目录；同一恢复入口还会固定原镜像，而非只恢复数据库
sudo bash embyrelay-upgrade.sh --restore /srv/media-relay/backups/upgrade-VERSION-TIMESTAMP --yes
```

恢复前核对备份属于本机原实例。恢复验证失败时应用保持停止，备份、事务和恢复命令保留；不要手动用旧程序打开升级后的数据库，不要 `down -v`。失败的单文件下载入口会将公共实现保留在 `/var/tmp/embyrelay-upgrade.XXXXXX/`，其提示的 Python 恢复命令仍可执行，不受退出清理影响。备份尚未完成但事务确认没有修改配置/数据时，恢复入口校验原配置及实际原容器，并恢复原运行状态；不使用部分归档覆盖数据。数据已可能修改而备份不完整时安全退出，需人工恢复。

空间要求：备份文件系统至少有原数据大小两倍且不低于 64 MiB 的空闲；数据文件系统保留足够恢复空间。不打印密码、Token、私钥或完整敏感配置。备份含这些敏感内容，请按凭据保管，不要上传到公开问题中。

退出码：0 成功或同版本跳过；1 系统/状态错误；2 参数、部署不支持或选择不明确；3 版本查询/镜像/认证/Docker 失败；4 备份、空间或 schema 不兼容；5 升级验收失败（已尝试回滚）；6 恢复失败或有未完成事务；7 实例锁冲突；128+信号为被中断并完成恢复处理。`--help` 列出全部参数。

## 原安装入口

```bash
sudo bash scripts/install.sh upgrade --directory /srv/media-relay --version 1.0.21 --yes
sudo bash scripts/install-agent.sh upgrade --directory /srv/media-node --version 1.0.21 --yes
```

两者调用同一公共升级事务。控制端重跑安装器时，已完成初始化且已清空初始密码的部署转入公共升级器；Agent 重跑安装器时，已完成注册且清空注册令牌的部署也转入公共升级入口；不再次注册已有节点。未完成首次安装仍沿用安装器的恢复流程。旧版脚本文件应重新下载/同步；已经下载在用户主机的历史脚本不会自动获得新实现。
