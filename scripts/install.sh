#!/usr/bin/env bash
set -euo pipefail
if [[ "$(uname -s)" != Linux || "$(id -u)" != 0 ]]; then
  echo '请在 Linux 服务器上以 root 运行（sudo bash install.sh）。' >&2
  exit 1
fi
if ! command -v python3 >/dev/null; then
  command -v apt-get >/dev/null || { echo '请先安装 Python 3。' >&2; exit 1; }
  apt-get update
  apt-get install -y python3 ca-certificates curl
fi
export EMBYRELAY_INSTALLER_DIRECTORY
EMBYRELAY_INSTALLER_DIRECTORY=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
exec python3 - "$@" <<'PY'
import argparse
import contextlib
import hashlib
import shlex
import fcntl
import getpass
import ipaddress
import json
import os
from pathlib import Path
import re
import shutil
import socket
import secrets
import tarfile
import subprocess
import sys
import tempfile
import time
import urllib.request

PROJECT = 'embyrelay-managed'
IMAGE = os.environ.get('RELAY_IMAGE', 'ecouus/embyrelay:latest')
VOLUMES = ['relay_data', 'postgres_data']
POSTGRES_IMAGE = 'postgres:17-alpine'


def ask(label, default=''):
    with open('/dev/tty', 'r') as tty, open('/dev/tty', 'w') as output:
        output.write(label + (f' [{default}]' if default else '') + ': ')
        output.flush()
        value = tty.readline()
        if not value:
            raise ValueError('输入已中断')
        return value.strip() or default


def password():
    with open('/dev/tty', 'w') as tty:
        first = getpass.getpass('管理员密码（8–72 字节，不回显）: ', stream=tty)
        second = getpass.getpass('再次输入密码: ', stream=tty)
    if first != second:
        raise ValueError('两次密码不一致')
    if not 8 <= len(first.encode()) <= 72 or any(c in first for c in '\n\r\0'):
        raise ValueError('密码需为 8–72 字节，不能含换行或 NUL')
    return first


def domain(value):
    value = value.lower().rstrip('.')
    if len(value) > 253 or '.' not in value or not all(
        re.fullmatch(r'[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?', p) for p in value.split('.')
    ):
        raise ValueError('只填写有效域名，例如 relay.example.com，不含协议、端口、路径')
    try:
        ipaddress.ip_address(value)
    except ValueError:
        return value
    raise ValueError('证书部署需要域名，不能填写 IP')


def write_private(path, text):
    fd, temp = tempfile.mkstemp(dir=path.parent, prefix='.' + path.name)
    try:
        with os.fdopen(fd, 'w') as f:
            f.write(text)
        os.replace(temp, path)
    finally:
        if os.path.exists(temp):
            os.unlink(temp)


def save(path, config):
    # JSON is valid YAML. Compose interpolates $ even in quoted strings.
    write_private(path, json.dumps(config, ensure_ascii=False, indent=2).replace('$', '$$') + '\n')


def configuration(host, username, secret, database='sqlite', database_password=None, database_port=15432):
    if database not in ('sqlite', 'postgres'):
        raise ValueError('数据库类型必须为 sqlite 或 postgres')
    config = {
        'name': PROJECT,
        'x-embyrelay-installer': {'version': 1, 'domain': host, 'admin': username, 'database': database},
        'services': {
            'embyrelay': {
                'image': IMAGE, 'network_mode': 'host',
                'environment': {'RELAY_LISTEN': '127.0.0.1:6689', 'RELAY_DATA_DIR': '/data',
                    'RELAY_ADMIN_USER': username, 'RELAY_ADMIN_PASSWORD': secret,
                    'RELAY_ALLOW_INSECURE': 'false', 'RELAY_TRUSTED_PROXIES': '127.0.0.1/32'},
                'volumes': ['relay_data:/data'], 'read_only': True, 'cap_drop': ['ALL'],
                'security_opt': ['no-new-privileges:true'], 'restart': 'unless-stopped',
                'stop_grace_period': '15s',
            },
        },
        'volumes': {'relay_data': {'name': PROJECT + '_relay_data'}},
    }
    app = config['services']['embyrelay']
    app['environment']['RELAY_DB_DRIVER'] = database
    if database == 'postgres':
        if not database_password or not re.fullmatch(r'[a-zA-Z0-9_-]{32,}', database_password):
            raise ValueError('数据库密码必须为至少 32 位安全随机字符')
        config['x-embyrelay-installer']['database_port'] = database_port
        app['environment']['RELAY_DATABASE_URL_FILE'] = '/run/secrets/database_url'
        app['secrets'] = ['database_url']
        app['depends_on'] = {'postgres': {'condition': 'service_healthy'}}
        config['services']['postgres'] = {
            'image': POSTGRES_IMAGE,
            'environment': {'POSTGRES_USER': 'embyrelay', 'POSTGRES_DB': 'embyrelay',
                            'POSTGRES_PASSWORD_FILE': '/run/secrets/postgres_password'},
            'secrets': ['postgres_password'],
            'ports': [f'127.0.0.1:{database_port}:5432'],
            'volumes': ['postgres_data:/var/lib/postgresql/data'],
            'restart': 'unless-stopped',
            'healthcheck': {'test': ['CMD-SHELL', 'pg_isready -U embyrelay -d embyrelay'],
                            'interval': '5s', 'timeout': '5s', 'retries': 12, 'start_period': '10s'},
            'stop_grace_period': '60s',
        }
        config['volumes']['postgres_data'] = {'name': PROJECT + '_postgres_data'}
        config['secrets'] = {
            'database_url': {'file': './secrets/database_url'},
            'postgres_password': {'file': './secrets/postgres_password'},
        }
    return config


def database_type(config):
    return config['x-embyrelay-installer'].get('database', 'sqlite')


def save_database_secrets(directory, secret, port):
    folder = directory / 'secrets'
    folder.mkdir(mode=0o700, exist_ok=True)
    os.chmod(folder, 0o700)
    write_private(folder / 'postgres_password', secret + '\n')
    write_private(folder / 'database_url',
                  f'postgres://embyrelay:{secret}@127.0.0.1:{port}/embyrelay?sslmode=disable\n')
    # The application image runs as uid 10001. Compose file secrets are bind
    # mounts, so ownership must be set on the source file itself.
    os.chown(folder / 'database_url', 10001, 10001)
    os.chmod(folder / 'database_url', 0o400)


def database_port_available(port):
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        try:
            sock.bind(('127.0.0.1', port))
        except OSError as error:
            raise ValueError(f'数据库端口 {port} 已被占用，请用 --database-port 指定其他端口') from error


def migrate_database(compose, directory, path, config, port):
    if database_type(config) != 'sqlite':
        raise ValueError('一键迁移当前仅支持 SQLite → PostgreSQL；反向迁移请使用离线迁移工具')
    pending = directory / 'docker-compose.migrate.yml'
    if pending.exists():
        candidate = load(pending)
        if candidate['x-embyrelay-installer']['domain'] != config['x-embyrelay-installer']['domain']:
            raise ValueError('迁移配置与当前部署不一致')
    else:
        existing = run(['docker', 'volume', 'ls', '--format', '{{.Name}}'], capture_output=True, text=True).stdout.splitlines()
        if PROJECT + '_postgres_data' in existing:
            raise ValueError('发现已有 PostgreSQL 数据卷，拒绝自动覆盖或接管')
        database_port_available(port)
        secret = secrets.token_urlsafe(48)
        metadata = config['x-embyrelay-installer']
        candidate = configuration(metadata['domain'], metadata['admin'], '', 'postgres', secret, port)
        candidate['x-embyrelay-installer']['migration_id'] = secrets.token_hex(24)
        # Preserve existing application image and operator settings.
        app = candidate['services']['embyrelay']
        original = json.loads(json.dumps(config['services']['embyrelay']))
        original['environment'].update({
            'RELAY_DB_DRIVER': 'postgres', 'RELAY_DATABASE_URL_FILE': '/run/secrets/database_url'})
        original['secrets'] = app['secrets']
        original['depends_on'] = app['depends_on']
        candidate['services']['embyrelay'] = original
        save_database_secrets(directory, secret, port)
        save(pending, candidate)
    next_compose = compose[:-1] + [str(pending)]
    # A committed copy can be resumed without copying data again.
    if candidate['x-embyrelay-installer'].get('migration_complete'):
        save(path, candidate)
        start(compose, path, candidate)
        pending.unlink()
        return
    if not candidate['x-embyrelay-installer'].get('migration_started'):
        backup(compose, directory, config)
    run(compose + ['stop', 'embyrelay'])
    copied = False
    try:
        run(next_compose + ['up', '-d', '--wait', 'postgres'])
        candidate['x-embyrelay-installer']['migration_started'] = True
        save(pending, candidate)
        run(next_compose + ['run', '--rm', '-T', '--no-deps', '--entrypoint',
                            '/usr/local/bin/embyrelay-db-migrate', 'embyrelay',
                            '--source-driver', 'sqlite', '--source', '/data/embyrelay.db',
                            '--target-driver', 'postgres', '--target-url-file',
                            '/run/secrets/database_url', '--offline', '--migration-id',
                            candidate['x-embyrelay-installer']['migration_id']])
        copied = True
        candidate['x-embyrelay-installer']['migration_complete'] = True
        save(pending, candidate)
        save(path, candidate)
        start(compose, path, candidate)
        pending.unlink()
    except BaseException:
        if not copied and not candidate['x-embyrelay-installer'].get('migration_started'):
            run(compose + ['start', 'embyrelay'])
            print('迁移未完成，原 SQLite 应用已恢复；迁移配置保留供重试。', file=sys.stderr)
        else:
            print('应用保持停止，原 SQLite 保留。请用相同目录运行 migrate-db 重试；已提交的复制不会重复执行。', file=sys.stderr)
        raise
    print('已切换到 PostgreSQL；原 SQLite 文件和迁移前备份保留。')



NGINX_TEMPLATE = 'map $http_upgrade $embyrelay_connection {\n    default upgrade;\n    \'\'      close;\n}\n\nserver {\n    listen 80;\n    server_name relay.example.com;\n\n    access_log off;\n\n    # 使用 webroot 方式签发证书时，保留验证目录\n    location ^~ /.well-known/acme-challenge/ {\n        root /var/www/html;\n    }\n    location / {\n        return 301 https://relay.example.com$request_uri;\n    }\n}\n\nserver {\n    listen 443 ssl;\n    server_name relay.example.com;\n\n    ssl_certificate     /etc/letsencrypt/live/relay.example.com/fullchain.pem;\n    ssl_certificate_key /etc/letsencrypt/live/relay.example.com/privkey.pem;\n    ssl_protocols TLSv1.2 TLSv1.3;\n\n    # 播放 URL 含访问密钥，不记录访问日志\n    access_log off;\n    add_header Referrer-Policy "no-referrer" always;\n    add_header X-Content-Type-Options "nosniff" always;\n\n    location / {\n        # 保留完整的 /r/... 播放路径\n        proxy_pass http://127.0.0.1:6689;\n        proxy_http_version 1.1;\n\n        # 保留端口，确保后台同源校验正确\n        proxy_set_header Host              $http_host;\n        proxy_set_header X-Real-IP         $remote_addr;\n        proxy_set_header X-Forwarded-For   $remote_addr;\n        proxy_set_header X-Forwarded-Proto $scheme;\n\n        # WebSocket\n        proxy_set_header Upgrade    $http_upgrade;\n        proxy_set_header Connection $embyrelay_connection;\n\n        # 流式转发，不缓存视频响应\n        proxy_buffering off;\n        proxy_request_buffering off;\n        proxy_cache off;\n\n        proxy_connect_timeout 10s;\n        proxy_read_timeout 3600s;\n        proxy_send_timeout 3600s;\n    }\n}\n'

def run(command, **kwargs):
    return subprocess.run(command, check=True, **kwargs)


def ensure_docker():
    if not shutil.which('docker'):
        release = Path('/etc/os-release').read_text()
        if not re.search(r'^ID=[\"\']?(debian|ubuntu)[\"\']?$', release, re.M):
            raise ValueError('自动安装 Docker 仅支持 Debian / Ubuntu；请先安装 Docker Engine 和 Compose v2')
        print('未检测到 Docker，将下载并运行 Docker 官方安装脚本，安装 Engine 与 Compose 插件。', flush=True)
        if ask('是否安装 Docker？输入 yes 继续', 'no') != 'yes':
            raise ValueError('请先安装 Docker Engine 与 Compose v2 后重试')
        run(['apt-get', 'update'])
        run(['apt-get', 'install', '-y', 'ca-certificates', 'curl'])
        with tempfile.TemporaryDirectory() as temp:
            script = str(Path(temp) / 'docker.sh')
            run(['curl', '-fsSL', '--retry', '3', 'https://get.docker.com', '-o', script])
            run(['sh', script])
        run(['systemctl', 'enable', '--now', 'docker'])
    run(['docker', 'compose', 'version'], stdout=subprocess.DEVNULL)
    run(['docker', 'info'], stdout=subprocess.DEVNULL)


def available_ports():
    for host, port, kind in [('127.0.0.1', 6689, socket.SOCK_STREAM)]:
        with socket.socket(socket.AF_INET, kind) as s:
            try:
                s.bind((host, port))
            except OSError as e:
                raise ValueError(f'端口 {port} 已占用或不可绑定，请先处理现有反代/服务：{e}') from e


def load(path):
    config = json.loads(path.read_text().replace('$$', '$'))
    if (config.get('name') != PROJECT or
            config.get('x-embyrelay-installer', {}).get('version') != 1):
        raise ValueError('该目录不是本脚本管理的部署，拒绝覆盖或卸载')
    return config


def healthy(compose, database=None):
    for _ in range(30):
        result = subprocess.run(compose + ['exec', '-T', 'embyrelay', 'wget', '-q', '-O',
                                '-', 'http://127.0.0.1:6689/healthz'],
                                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
        if result.returncode == 0:
            if database == 'postgres':
                try:
                    active = json.loads(result.stdout).get('database')
                except (ValueError, TypeError):
                    active = None
                if active != 'postgres':
                    run(compose + ['stop', 'embyrelay'])
                    raise ValueError('应用镜像未启用 PostgreSQL；已停止应用，请使用包含双数据库支持的新镜像')
            return
        time.sleep(2)
    raise ValueError('后端未就绪；配置和数据已保留。使用脚本 logs 查看日志，再运行 install 重试')


def start(compose, path, config):
    run(compose + ['config', '--quiet'])
    run(compose + ['up', '-d', '--remove-orphans'])
    healthy(compose, database_type(config))
    env = config['services']['embyrelay']['environment']
    if env.get('RELAY_ADMIN_PASSWORD'):
        env['RELAY_ADMIN_PASSWORD'] = ''
        save(path, config)
        run(compose + ['up', '-d', '--force-recreate', 'embyrelay'])
        healthy(compose, database_type(config))


def check_https(host):
    print('正在验证 HTTPS（最多约 90 秒）；检查 Nginx 的证书与响应…', flush=True)
    deadline = time.monotonic() + 90
    while time.monotonic() < deadline:
        try:
            with urllib.request.urlopen('https://' + host + '/healthz', timeout=5) as response:
                if response.status == 200:
                    print('HTTPS 访问验证通过：https://' + host)
                    return True
        except (OSError, ValueError):
            pass
        time.sleep(3)
    print('容器已启动，但 HTTPS 尚未验证通过。请检查域名 A/AAAA、云安全组和防火墙的 TCP 80/443。\n'
          '请查看 /var/log/letsencrypt/letsencrypt.log 和 Nginx 日志；修复后重新运行 install。')
    return False


def backup(compose, directory, config=None, automatic=False):
    backups = directory / 'backups'
    backups.mkdir(mode=0o700, exist_ok=True)
    destination = backups / (('auto-' if automatic else '') + time.strftime('%Y%m%d-%H%M%S') + '-' + str(time.time_ns()) + '.tar.gz')
    run(compose + ['stop', 'embyrelay'])
    try:
        if config is not None and database_type(config) == 'postgres':
            with tempfile.TemporaryDirectory(dir=backups) as temp:
                temp = Path(temp)
                with (temp / 'database.dump').open('xb') as output:
                    run(compose + ['exec', '-T', 'postgres', 'pg_dump', '-U', 'embyrelay',
                                   '-d', 'embyrelay', '-Fc', '--no-owner', '--no-acl'], stdout=output)
                with (temp / 'data.tar.gz').open('xb') as output:
                    run(compose + ['run', '--rm', '-T', '--no-deps', '--entrypoint', 'tar',
                                   'embyrelay', 'czf', '-', '-C', '/data', '.'], stdout=output)
                with tarfile.open(destination, 'x:gz') as archive:
                    os.chmod(destination, 0o600)
                    for name in ['database.dump', 'data.tar.gz']:
                        archive.add(temp / name, arcname=name)
                    archive.add(directory / 'docker-compose.yml', arcname='docker-compose.yml')
                    archive.add(directory / 'secrets', arcname='secrets')
        else:
            with destination.open('xb') as output:
                os.chmod(destination, 0o600)
                run(compose + ['run', '--rm', '-T', '--no-deps', '--entrypoint', 'tar',
                               'embyrelay', 'czf', '-', '-C', '/data', '.'], stdout=output)
    except BaseException:
        destination.unlink(missing_ok=True)
        raise
    finally:
        run(compose + ['start', 'embyrelay'])
    print('升级前数据备份（含数据库和密钥）：' + str(destination))
    return destination


def prune_automatic_backups(directory, keep=5):
    backups = sorted((directory / 'backups').glob('auto-*.tar.gz'), key=lambda item: item.stat().st_mtime, reverse=True)
    for old in backups[keep:]:
        old.unlink()


def reset_password(compose, username, secret):
    # Send the secret on stdin, never in argv or Docker container configuration.
    run(compose + ['exec', '-T', 'embyrelay', 'sh', '-c',
        'IFS= read -r RELAY_ADMIN_PASSWORD; export RELAY_ADMIN_PASSWORD; '
        'exec /usr/local/bin/embyrelay reset-password "$1"', 'sh', username],
        input=secret + '\n', text=True)


def validate_data_archive(path):
    # tar runs in the application container; nevertheless reject paths and
    # special files that could overwrite files outside the data volume.
    with tarfile.open(path, 'r:gz') as archive:
        for member in archive.getmembers():
            name = Path(member.name)
            if name.is_absolute() or '..' in name.parts or not (member.isfile() or member.isdir()):
                raise ValueError('备份含不安全路径或特殊文件，拒绝恢复')


def restore(compose, directory, config, backup_file, automatic=False, image=None, version=None):
    source = Path(backup_file).expanduser().resolve()
    if not source.is_file():
        raise ValueError('备份文件不存在')
    with tempfile.TemporaryDirectory(dir=directory) as temp:
        temp = Path(temp)
        data = source
        dump = None
        if database_type(config) == 'postgres':
            with tarfile.open(source, 'r:gz') as archive:
                saved = json.load(archive.extractfile('docker-compose.yml'))
                if database_type(saved) != 'postgres':
                    raise ValueError('备份数据库类型与当前部署不一致')
                for name in ['database.dump', 'data.tar.gz']:
                    member = archive.getmember(name)
                    if not member.isfile():
                        raise ValueError('备份内容无效')
                    with archive.extractfile(member) as reader, (temp / name).open('xb') as writer:
                        shutil.copyfileobj(reader, writer)
            data, dump = temp / 'data.tar.gz', temp / 'database.dump'
        validate_data_archive(data)
        if database_type(config) == 'sqlite':
            with tarfile.open(data, 'r:gz') as archive:
                if 'embyrelay.db' not in {str(Path(m.name)) for m in archive.getmembers()}:
                    raise ValueError('SQLite 备份缺少数据库文件，或备份类型不匹配')
        if not automatic and ask('恢复将覆盖当前数据；输入 RESTORE 继续', '') != 'RESTORE':
            print('已取消恢复')
            return
        if not automatic:
            backup(compose, directory, config)
        if image:
            override = temp / 'rollback-compose.json'
            write_private(override, json.dumps({'services': {'embyrelay': {'image': image, 'pull_policy': 'never'}}}))
            compose = compose + ['-f', str(override)]
        run(compose + ['stop', 'embyrelay'])
        try:
            if dump is not None:
                with dump.open('rb') as reader:
                    run(compose + ['exec', '-T', 'postgres', 'pg_restore', '-U', 'embyrelay',
                                   '-d', 'embyrelay', '--clean', '--if-exists', '--single-transaction',
                                   '--no-owner', '--no-acl'], stdin=reader)
            with data.open('rb') as reader:
                run(compose + ['run', '--rm', '-T', '--no-deps', '--entrypoint', 'sh',
                               'embyrelay', '-c', 'rm -f /data/embyrelay.db-wal /data/embyrelay.db-shm; '
                               'exec tar xzf - -C /data'], stdin=reader)
        except BaseException:
            print('恢复未完成，应用保持停止。请使用恢复前备份处理后再启动。', file=sys.stderr)
            raise
        if image:
            run(compose + ['up', '-d', '--no-deps', '--force-recreate', '--pull', 'never', 'embyrelay'])
        else:
            run(compose + ['start', 'embyrelay'])
        healthy(compose, database_type(config))
        if image:
            if service_image(compose) != image:
                run(compose + ['stop', 'embyrelay'])
                raise ValueError('回滚镜像校验失败；应用已停止')
            if version is not None and service_version(compose) != version:
                run(compose + ['stop', 'embyrelay'])
                raise ValueError('回滚版本校验失败；应用已停止')
        print('数据库和主密钥已恢复。')


def service_image(compose):
    container = subprocess.check_output(compose + ['ps', '-a', '-q', 'embyrelay'], text=True).strip()
    if not container or '\n' in container:
        raise ValueError('无法确定升级前的唯一应用容器')
    return subprocess.check_output(['docker', 'inspect', '--format', '{{.Image}}', container], text=True).strip()


def service_version(compose):
    health = subprocess.check_output(compose + ['exec', '-T', 'embyrelay', 'wget', '-q', '-O', '-',
                                              'http://127.0.0.1:6689/healthz'], text=True)
    version = json.loads(health).get('version')
    if not isinstance(version, str) or not version:
        raise ValueError('无法读取升级前应用版本')
    return version


def upgrade(compose, directory, path, config, options=None):
    shared_upgrade(directory, 'control', options)


def uninstall(compose, directory):
    if ask('卸载将停止本项目容器，输入 yes 确认', 'no') != 'yes':
        print('已取消卸载')
        return
    delete = ask('默认保留数据；永久删除项目数据卷请输入 DELETE', '') == 'DELETE'
    pending = directory / 'docker-compose.migrate.yml'
    if pending.exists():
        load(pending)  # Only manage a recognized installer configuration.
        run(compose[:-1] + [str(pending), 'down'] + (['--volumes'] if delete else []))
    run(compose + ['down'] + (['--volumes'] if delete else []))
    if delete:
        # Only remove files owned by this installer; never recursively delete the directory.
        for name in ['docker-compose.yml', 'docker-compose.migrate.yml', 'Caddyfile', 'secrets/database_url', 'secrets/postgres_password']:
            (directory / name).unlink(missing_ok=True)
        secret_directory = directory / 'secrets'
        if secret_directory.exists() and not any(secret_directory.iterdir()):
            secret_directory.rmdir()
        print('本项目数据卷已删除；宿主机 Nginx 配置、证书及历史备份保留，避免影响共用站点。')
    else:
        print('容器已卸载，配置、数据、证书均保留；运行 install 可恢复。')


def nginx_tokens(text):
    # Keep source offsets so unrelated virtual hosts survive edits byte-for-byte.
    pattern = r'''\s+|\#[^\n]*|"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[{};]|[^\s{};#"']+'''
    tokens = []
    for match in re.finditer(pattern, text):
        raw = match.group()
        if raw.isspace() or raw.startswith('#'):
            continue
        tokens.append((raw.strip('\"\''), match.start(), match.end()))
    return tokens


def server_blocks(text):
    tokens = nginx_tokens(text)
    result = []
    for i, token in enumerate(tokens[:-1]):
        if token[0] != 'server' or tokens[i + 1][0] != '{':
            continue
        depth = 1
        j = i + 2
        while j < len(tokens) and depth:
            depth += (tokens[j][0] == '{') - (tokens[j][0] == '}')
            j += 1
        if depth:
            raise ValueError('Nginx 配置块不完整，停止修改')
        result.append((token[1], tokens[j - 1][2], text[token[1]:tokens[j - 1][2]]))
    return result


def names(block):
    values = [t[0] for t in nginx_tokens(block)]
    if 'server_name' not in values:
        return []
    start = values.index('server_name') + 1
    return values[start:values.index(';', start)]


def active_nginx_files():
    result = run(['nginx', '-T'], capture_output=True, text=True)
    # nginx -T lists exactly the included files, including sites-enabled symlinks.
    return list(dict.fromkeys(Path(p).resolve() for p in
        re.findall(r'^# configuration file (.+):$', result.stdout, re.M)))


def normalized(text):
    return [t[0] for t in nginx_tokens(text)]


def nginx_plan(host, files):
    desired = NGINX_TEMPLATE.replace('relay.example.com', host)
    expected = [normalized(b[2]) for b in server_blocks(desired)]
    matches = []
    for path in files:
        text = path.read_text()
        for start, end, block in server_blocks(text):
            if host in names(block):
                matches.append((path, start, end, block))
    if len(matches) == 2 and sorted(normalized(m[3]) for m in matches) == sorted(expected):
        # The map must also exist in the active http configuration.
        map_text = desired[:desired.index('server {')]
        combined = ' '.join(' '.join(normalized(p.read_text())) for p in files)
        if ' '.join(normalized(map_text)) in combined:
            return {}, True
    edits = {}
    for path, start, end, block in matches:
        if names(block) != [host]:
            raise ValueError(f'{path} 中该域名与其他域名共用 server；请先拆分站点，避免覆盖其他域名')
    for path in {m[0] for m in matches}:
        text = path.read_text()
        for _, start, end, _ in sorted((m for m in matches if m[0] == path), key=lambda m: m[1], reverse=True):
            text = text[:start] + text[end:]
        edits[path] = text
    # Use a unique map variable to avoid redefining an existing README map.
    variable = 'embyrelay_connection_' + hashlib.sha256(host.encode()).hexdigest()[:12]
    target = Path('/etc/nginx/conf.d/embyrelay-' + host + '.conf')
    if target.exists() and target not in edits and target not in files:
        raise ValueError(f'{target} 存在但未被 Nginx 加载，请检查 include 配置')
    content = desired.replace('$embyrelay_connection', '$' + variable)
    # Repeated runs recognize our exact generated config too.
    if len(matches) == 2 and target in files and normalized(target.read_text()) == normalized(content):
        return {}, True
    if target.exists() and target not in edits:
        raise ValueError(f'{target} 已存在其他配置，拒绝覆盖')
    remaining = edits.get(target, '')
    # Drop only our own prior generated map when replacing a managed file.
    if remaining.strip() and normalized(remaining) != normalized(content[:content.index('server {')]):
        raise ValueError(f'{target} 含其他配置，请先拆分文件')
    edits[target] = content
    return edits, False


def apply_nginx(edits, directory):
    if not edits:
        return
    originals = {p: p.read_bytes() if p.exists() else None for p in edits}
    backup_dir = directory / 'backups' / ('nginx-' + str(time.time_ns()))
    backup_dir.mkdir(parents=True, mode=0o700)
    write_private(backup_dir / 'manifest.json', json.dumps({str(i): str(p) for i, p in enumerate(edits)}))
    for i, (p, data) in enumerate(originals.items()):
        if data is not None:
            (backup_dir / str(i)).write_bytes(data)
    try:
        for p, text in edits.items():
            p.parent.mkdir(parents=True, exist_ok=True)
            write_private(p, text)
        run(['nginx', '-t'])
        loaded = active_nginx_files()
        if any(p.resolve() not in loaded for p in edits):
            raise ValueError('生成的配置未被 nginx.conf include，已回滚')
        run(['systemctl', 'reload', 'nginx'])
    except BaseException:
        for p, data in originals.items():
            if data is None:
                p.unlink(missing_ok=True)
            else:
                write_private(p, data.decode())
        run(['nginx', '-t'])
        run(['systemctl', 'reload', 'nginx'])
        raise
    print('Nginx 配置已更新；原配置备份：' + str(backup_dir))


def ensure_nginx_certbot():
    missing = [name for name in ['nginx', 'certbot'] if not shutil.which(name)]
    if missing:
        if not shutil.which('apt-get'):
            raise ValueError('请先安装 Nginx、Certbot 和 Certbot nginx 插件；自动安装仅支持 apt 系统')
        run(['apt-get', 'update'])
        run(['apt-get', 'install', '-y'] + missing)
    plugins = run(['certbot', 'plugins', '--text'], capture_output=True, text=True).stdout
    if not re.search(r'\* nginx\b', plugins):
        # Do not mix apt plugins with a snap/pip Certbot installation.
        if Path(shutil.which('certbot')).resolve() != Path('/usr/bin/certbot'):
            raise ValueError('已有 Certbot 缺少 nginx 插件，请通过其原安装方式安装插件后重试')
        run(['apt-get', 'install', '-y', 'python3-certbot-nginx'])
    run(['nginx', '-t'])
    run(['systemctl', 'enable', '--now', 'nginx'])


def setup_nginx(compose, directory, path, config):
    host = domain(config['x-embyrelay-installer']['domain'])
    if 'caddy' in config['services']:
        print('迁移旧版 Caddy：保留应用及证书卷，停止本项目 Caddy 释放端口。')
        run(compose + ['stop', 'caddy'])
        config['services'].pop('caddy')
        save(path, config)
    ensure_nginx_certbot()
    files = active_nginx_files()
    # Plan before certificate operations, so shared virtual hosts fail safely.
    nginx_plan(host, files)
    live = Path('/etc/letsencrypt/live') / host
    renewal = Path('/etc/letsencrypt/renewal') / (host + '.conf')
    valid = False
    if (live / 'fullchain.pem').exists() and renewal.exists():
        valid = subprocess.run(['openssl', 'x509', '-in', str(live / 'fullchain.pem'),
            '-noout', '-checkend', '2592000', '-checkhost', host],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0
    if not valid:
        print('通过 Certbot nginx 插件申请/续签证书；需同意 Let’s Encrypt 服务条款。')
        with open('/dev/tty', 'r') as tty:
            run(['certbot', 'certonly', '--nginx', '--cert-name', host, '-d', host,
                 '--keep-until-expiring'], stdin=tty)
    else:
        print('已有对应域名的有效 Certbot 证书，跳过重复申请。')
    # Certbot may have temporarily adjusted configuration; rediscover before editing.
    edits, correct = nginx_plan(host, active_nginx_files())
    if correct:
        print('域名反代配置已符合 README 模板，保持原配置。')
    else:
        apply_nginx(edits, directory)
    # A dedicated cron entry is installed only when no packaged renewal scheduler is active.
    timers = subprocess.run(['systemctl', 'list-timers', '--all', '--no-legend'], capture_output=True, text=True)
    cron = Path('/etc/cron.d/certbot')
    if not re.search(r'(certbot|snap\.certbot).*timer', timers.stdout) and not cron.exists():
        if not shutil.which('cron'):
            run(['apt-get', 'install', '-y', 'cron'])
        run(['systemctl', 'enable', '--now', 'cron'])
        entry = Path('/etc/cron.d/embyrelay-certbot')
        text = '17 3,15 * * * root ' + shlex.quote(shutil.which('certbot')) + ' renew --quiet\n'
        if not entry.exists() or entry.read_text() != text:
            write_private(entry, text)
            entry.chmod(0o644)
    hook = Path('/etc/letsencrypt/renewal-hooks/deploy/embyrelay-nginx')
    hook.parent.mkdir(parents=True, exist_ok=True)
    text = '#!/bin/sh\nset -eu\n' + shlex.quote(shutil.which('nginx')) + ' -t\nsystemctl reload nginx\n'
    if not hook.exists() or hook.read_text() != text:
        write_private(hook, text)
        hook.chmod(0o700)


def shared_upgrade(directory, role, options=None):
    script = Path(os.environ.get('EMBYRELAY_INSTALLER_DIRECTORY', '.')) / 'upgrade.py'
    temporary = None
    if not script.is_file():
        temporary = Path(tempfile.mkdtemp(prefix='embyrelay-upgrade-', dir='/var/tmp'))
        script = temporary / 'upgrade.py'
        try:
            with urllib.request.urlopen('https://raw.githubusercontent.com/ecouus/EmbyRelay/main/scripts/upgrade.py', timeout=30) as response:
                write_private(script, response.read().decode())
        except Exception:
            raise ValueError('公共升级器下载失败；已有部署未修改')
    command = ['python3', str(script)]
    if directory is not None:
        command += ['--directory', str(directory)]
    if role:
        command += ['--role', role]
    command += options or []
    # The installer itself reads Python from stdin; give the upgrader a real TTY.
    with contextlib.ExitStack() as stack:
        tty = None
        if os.isatty(1):
            try:
                tty = stack.enter_context(open('/dev/tty'))
            except OSError:
                pass
        result = subprocess.run(command, stdin=tty)
    if temporary and result.returncode in (0, 8):
        shutil.rmtree(temporary)
    if result.returncode == 8 and '--detect-only' in (options or []):
        return False
    if result.returncode:
        raise SystemExit(result.returncode)
    return True


def route_upgrade(args):
    if args.action not in (None, 'upgrade'):
        return False
    directory = Path(args.directory).expanduser().resolve() if args.directory else None
    options = (['--version', args.version] if args.version else []) + (['--yes'] if args.yes else [])
    if args.public_url:
        options += ['--public-url', args.public_url]
    if args.instance:
        options += ['--instance', args.instance]
    if args.action is None:
        if not shutil.which('docker'):
            return False
        # An unfinished first control install must keep its recovery path.
        path = (directory or Path('/opt/embyrelay')) / 'docker-compose.yml'
        if path.is_file():
            try:
                config = json.loads(path.read_text())
            except (ValueError, OSError):
                config = {}
            if config.get('x-embyrelay-installer') and config.get('services', {}).get('embyrelay', {}).get('environment', {}).get('RELAY_ADMIN_PASSWORD'):
                return False
        if not shared_upgrade(directory, args.role, options + ['--detect-only']):
            if args.instance or args.role or args.version or args.public_url:
                raise ValueError('未发现所指定的已有部署；请核对目录、角色或实例，不会创建新部署')
            return False
    if args.database:
        raise ValueError('已有部署升级保持原数据库类型；数据库迁移请使用 migrate-db')
    shared_upgrade(directory, args.role, options)
    return True


def main():
    parser = argparse.ArgumentParser(description='EmbyRelay 统一安装/升级入口：已有控制端或 Agent 自动识别；首次安装控制端（Linux root）')
    parser.add_argument('action', nargs='?', choices=['install', 'upgrade', 'backup', 'restore', 'migrate-db', 'password', 'uninstall', 'status', 'logs'])
    parser.add_argument('--directory', help='已有部署自动发现；首次安装控制端默认 /opt/embyrelay')
    parser.add_argument('--role', choices=['control', 'agent'], help='筛选升级角色')
    parser.add_argument('--instance', help='筛选升级实例 PROJECT/SERVICE')
    parser.add_argument('--database', choices=['sqlite', 'postgres'], help='首次安装的数据库类型')
    parser.add_argument('--database-port', type=int, default=15432, help='PostgreSQL 本机端口（默认 15432）')
    parser.add_argument('--version', help='upgrade 目标版本')
    parser.add_argument('--public-url', help='升级后验收的 HTTPS 入口')
    parser.add_argument('--yes', action='store_true', help='upgrade 非交互确认')
    parser.add_argument('--backup-file', help='restore 操作的备份文件')
    args = parser.parse_args()
    os.umask(0o077)
    if os.geteuid() != 0 or sys.platform != 'linux':
        raise ValueError('请在 Linux 服务器上以 root 运行')
    if route_upgrade(args):
        return
    with open('/var/lock/embyrelay-managed.lock', 'w') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise ValueError('另一个管理脚本正在运行')
        action = args.action
        if not action:
            print('EmbyRelay 管理\n1. 安装 / 恢复启动\n2. 升级\n3. 修改管理员密码\n4. 卸载\n5. 状态\n6. 日志\n7. 备份\n8. 恢复备份\n9. SQLite 迁移到 PostgreSQL\n0. 退出')
            selected = ask('请选择', '1')
            if selected == '0':
                return
            action = {'1':'install', '2':'upgrade', '3':'password', '4':'uninstall', '5':'status', '6':'logs', '7':'backup', '8':'restore', '9':'migrate-db'}.get(selected)
            if not action:
                raise ValueError('无效选项')
        directory = Path(args.directory or '/opt/embyrelay').expanduser().resolve()
        if action == 'upgrade':
            options = (['--version', args.version] if args.version else []) + (['--yes'] if args.yes else [])
            if args.public_url:
                options += ['--public-url', args.public_url]
            shared_upgrade(Path(args.directory).resolve() if args.directory else None, args.role, options + (['--instance', args.instance] if args.instance else []))
            return
        path = directory / 'docker-compose.yml'
        config = load(path) if path.exists() else None
        pending = directory / 'docker-compose.migrate.yml'
        if pending.exists() and load(pending)['x-embyrelay-installer'].get('migration_started'):
            if action not in ('migrate-db', 'status', 'logs'):
                raise ValueError('数据库迁移尚未收尾，请用相同目录运行 migrate-db 重试')
        if not 1 <= args.database_port <= 65535:
            raise ValueError('数据库端口需为 1–65535')
        if config is not None and action != 'migrate-db' and args.database and args.database != database_type(config):
            raise ValueError('已有部署不能直接更换数据库；请先迁移数据，当前配置未修改')
        if action != 'install' and config is None:
            raise ValueError('未发现本脚本管理的配置，请指定正确的 --directory')
        if action == 'install' and config is not None and not config['services']['embyrelay']['environment'].get('RELAY_ADMIN_PASSWORD'):
            options = (['--version', args.version] if args.version else []) + (['--yes'] if args.yes else [])
            shared_upgrade(directory, 'control', options)
            return
        ensure_docker()
        directory.mkdir(parents=True, exist_ok=True)
        instance_lock = open(directory / '.install.lock', 'a')
        try:
            fcntl.flock(instance_lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise ValueError('此目录已有安装/升级任务运行')
        compose = ['docker', 'compose', '--project-name', PROJECT, '--project-directory', str(directory), '-f', str(path)]
        if action == 'install':
            if config is None:
                if directory.exists() and any(p.name not in ('backups', '.install.lock') for p in directory.iterdir()):
                    raise ValueError('安装目录非空；请使用新的空目录，避免覆盖已有文件')
                # Do not silently adopt volumes belonging to another directory/old deployment.
                existing = run(['docker', 'volume', 'ls', '--format', '{{.Name}}'], capture_output=True, text=True).stdout.splitlines()
                if any(PROJECT + '_' + v in existing for v in VOLUMES):
                    raise ValueError('检测到已有受管数据卷，请使用原部署目录恢复，不能重复初始化')
                available_ports()
                print('请先将域名直接解析到本机公网 IP，开放 TCP 80/443；错误的 AAAA 记录需修正。')
                host = domain(ask('域名'))
                username = ask('管理员用户名', 'admin')
                if not username or len(username) > 128 or any(ord(c) < 32 for c in username):
                    raise ValueError('管理员用户名无效')
                secret = password()
                selected_db = args.database
                if selected_db is None:
                    print('数据库选择：1. SQLite（默认，单机文件）  2. PostgreSQL（独立容器）')
                    selected_db = {'1': 'sqlite', '2': 'postgres'}.get(ask('请选择数据库', '1'))
                    if selected_db is None:
                        raise ValueError('无效的数据库选项')
                db_password = secrets.token_urlsafe(48) if selected_db == 'postgres' else None
                if selected_db == 'postgres':
                    database_port_available(args.database_port)
                config = configuration(host, username, secret, selected_db, db_password, args.database_port)
                directory.mkdir(mode=0o700, parents=True, exist_ok=True)
                os.chmod(directory, 0o700)
                if selected_db == 'postgres':
                    save_database_secrets(directory, db_password, args.database_port)
                save(path, config)
                print('首次配置已保存；失败后使用相同目录重试，不会覆盖账号或数据。', flush=True)
            setup_nginx(compose, directory, path, config)
            run(compose + ['pull'])
            start(compose, path, config)
            check_https(config['x-embyrelay-installer']['domain'])
            print('管理员用户名：' + config['x-embyrelay-installer']['admin'] + '（密码为你输入的密码）')
        elif action == 'upgrade':
            upgrade(compose, directory, path, config)
        elif action == 'backup':
            backup(compose, directory, config)
        elif action == 'migrate-db':
            if args.database not in (None, 'postgres'):
                raise ValueError('一键迁移仅支持 --database postgres')
            if database_type(config) == 'postgres' and pending.exists():
                start(compose, path, config)
                pending.unlink()
            else:
                migrate_database(compose, directory, path, config, args.database_port)
        elif action == 'restore':
            if not args.backup_file:
                args.backup_file = ask('备份文件路径')
            restore(compose, directory, config, args.backup_file)
        elif action == 'password':
            username = ask('需要修改的管理员用户名', config['x-embyrelay-installer']['admin'])
            reset_password(compose, username, password())
            print('管理员密码已修改，旧会话已撤销；两步验证设置保持不变。')
        elif action == 'uninstall':
            uninstall(compose, directory)
        elif action == 'status':
            print('数据库：' + database_type(config))
            run(compose + ['ps', '-a'])
            check_https(config['x-embyrelay-installer']['domain'])
        elif action == 'logs':
            run(compose + ['logs', '--tail', '100'])
        print('部署目录：' + str(directory))


if __name__ == '__main__':
    try:
        main()
    except KeyboardInterrupt:
        print('\n已中断；已有配置和数据保留。', file=sys.stderr)
        sys.exit(130)
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        print('操作失败：' + str(error), file=sys.stderr)
        sys.exit(1)
PY
