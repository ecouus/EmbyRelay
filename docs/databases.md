# SQLite 与 PostgreSQL 部署

控制端支持 SQLite 和 PostgreSQL。默认 SQLite，已有部署升级时保持原数据库类型。Relay Agent 继续使用本地 SQLite，保存配置缓存、已消费流量和预留额度。

## 首次安装

交互安装会询问数据库类型，也可直接指定：

```sh
sudo bash scripts/install.sh install --database sqlite
sudo bash scripts/install.sh install --database postgres
```

`--database` 只指定数据库选择；域名和管理员密码仍通过交互输入。PostgreSQL 使用独立容器及 `postgres_data` 数据卷，端口默认仅绑定 `127.0.0.1:15432`，不对公网开放。端口冲突时可指定：

```sh
sudo bash scripts/install.sh install --database postgres --database-port 25432
```

应用仍使用 host 网络，与宿主机 Nginx 的连接方式不变。PostgreSQL 镜像固定为 `postgres:17-alpine` 大版本，拉取升级可更新该大版本的补丁，不自动跨大版本升级。数据库密码自动生成；Compose 配置只引用凭据文件，不包含密码内容。

部署目录默认 `/opt/embyrelay`：

```text
docker-compose.yml
secrets/database_url         # 应用连接凭据，uid 10001 可读
secrets/postgres_password    # PostgreSQL 初始化凭据
backups/
```

`relay_data` 在 PostgreSQL 模式下仍保存主密钥等本地文件，不能删除。数据库和主密钥必须一起保留，否则无法解密源站凭据。

重复运行 `install` 和 `upgrade` 沿用已保存的选择、密码和数据卷。已有部署不能通过 `install --database postgres` 直接切换到空库。

新脚本需要配合包含双数据库支持的应用镜像。脚本检查健康接口返回的实际数据库类型，旧镜像若忽略 PostgreSQL 配置，会停止应用并报错，不会把 SQLite 模式误报为部署成功。

## 备份与恢复

```sh
sudo bash scripts/install.sh backup
sudo bash scripts/install.sh restore --backup-file /opt/embyrelay/backups/BACKUP.tar.gz
```

SQLite 备份保留原数据目录归档格式。PostgreSQL 备份包含 `pg_dump -Fc` 导出、应用数据目录、Compose 配置及连接凭据。备份目录与归档限制访问权限，归档含密码及主密钥，请按凭据文件保存。

备份期间控制端会短暂停止，然后恢复运行。升级前自动备份。恢复先验证归档，再要求输入 `RESTORE`，自动生成当前数据的备份，停止应用后恢复数据库与应用文件。恢复 PostgreSQL 使用单事务 `pg_restore`；恢复失败时应用保持停止，避免在数据库与密钥不一致时继续工作。

恢复命令用于同种数据库类型的已有部署；它不恢复旧容器镜像、不替换当前数据库密码，也不自动迁移 PostgreSQL 大版本。全新服务器恢复时，先安装相同数据库模式和兼容版本，然后恢复备份；新生成的数据库密码可继续使用。

所有命令支持 `--directory` 指定原部署目录。

## 已有 SQLite 迁移到 PostgreSQL

先升级到包含迁移工具的应用镜像，再执行：

```sh
sudo bash scripts/install.sh migrate-db --database postgres
```

脚本检查目标卷、备份原数据、停止控制端、启动 PostgreSQL，并运行离线复制工具。复制在一个目标事务中完成，校验各表行数，保留账号、登录凭据、配置、用量、额度、会话及审计记录。目标非空时拒绝覆盖。主密钥继续使用原 `relay_data` 中的文件。

迁移成功后才保存 PostgreSQL 配置并启动应用。原 SQLite 文件和迁移前备份均保留。

迁移过程中应暂停播放；订阅 Agent 在控制端停止时无法进行扣费授权。迁移被中断后，使用同一目录重新执行 `migrate-db`。一旦开始复制，应用保持停止，直到迁移收尾；目标数据库的事务提交标记使已完成的复制不会重复执行。

迁移后不要直接切回旧 SQLite 文件继续写入，因为旧库不包含迁移后的新增数据。

## 离线复制工具

镜像包含 `/usr/local/bin/embyrelay-db-migrate`，本地可用 `go build ./cmd/dbmigrate` 构建。工具支持 SQLite → PostgreSQL 和 PostgreSQL → 新 SQLite 文件；停止相关控制端后使用 `--offline`。

```sh
embyrelay-db-migrate --offline \
  --source-driver sqlite --source /data/embyrelay.db \
  --target-driver postgres --target-url-file /run/secrets/database_url

embyrelay-db-migrate --offline \
  --source-driver postgres --source-url-file /run/secrets/database_url \
  --target-driver sqlite --target /data/restored.db
```

源库需升级到当前 schema 版本。工具不修改源库、不切换应用配置、不复制主密钥。反向复制后的 SQLite 文件需由管理员配合原主密钥和应用配置完成切换。一键脚本的自动切换目前仅支持 SQLite → PostgreSQL。

## 手动运行控制端

默认配置保持兼容：

```text
RELAY_DB_DRIVER=sqlite
RELAY_DATA_DIR=/data
```

PostgreSQL 配置：

```text
RELAY_DB_DRIVER=postgres
RELAY_DATABASE_URL_FILE=/run/secrets/database_url
RELAY_DATA_DIR=/data
```

凭据文件是一行 PostgreSQL 连接 URL。也支持 `RELAY_DATABASE_URL`；文件配置优先。只有 SQLite 模式使用 `/data/embyrelay.db`。未知数据库类型或 PostgreSQL 连接失败会阻止启动，不自动退回 SQLite。

当前支持单个控制端实例。使用 PostgreSQL 不代表已经支持多个控制端；配置缓存、运行状态及部分锁仍在进程内。

## 流量写入与性能

上下行统计使用实际应用层字节。入站计量合并到下一次出站预留事务，响应结束时补写剩余入站；会话进度最多每秒更新一次，结束时记录最终值。异常进程退出可能丢失尚未落库的入站统计或最近会话进度，严格出站额度仍在发送前持久化预留。

套餐扣费及严格路由额度仍按发送块验证；PostgreSQL 不会消除每块订阅 RPC 或所有进程内锁。容量应通过实际码率与并发播放压测确认，不能仅凭数据库类型判断。

## 验证

```sh
go test -race ./...
python3 -m unittest discover -s scripts -p 'test_*.py'

# 使用专用测试数据库；每个测试创建并清理独立 schema。
EMBYRELAY_TEST_POSTGRES_URL='postgres://USER:PASSWORD@127.0.0.1:15432/TEST_DB?sslmode=disable' \
  go test -race ./internal/control ./internal/database
```

PostgreSQL 测试账户需允许创建 schema 和执行迁移。测试覆盖现有控制端行为，以及跨数据库复制、大整数、空值、审计序列、非空目标拒绝与提交后重试。

本地构建应用镜像后，可运行临时 Docker 项目的完整生命周期测试：

```sh
docker build -f deploy/Dockerfile -t embyrelay:postgres-test .
EMBYRELAY_DOCKER_TEST=1 EMBYRELAY_TEST_IMAGE=embyrelay:postgres-test \
  python3 -m unittest scripts/test_database_deployment.py
```

容器测试覆盖实际 PostgreSQL 部署、备份恢复、密码重置和 SQLite 迁移中断重试，结束后删除临时容器与数据卷。测试在 Docker Desktop 下使用 bridge 网络，生产安装器保持 host 网络。
