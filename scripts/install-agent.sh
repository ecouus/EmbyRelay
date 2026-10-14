#!/usr/bin/env bash
set -euo pipefail
command -v python3 >/dev/null || { echo '需要 Python 3：apt-get update && apt-get install -y python3' >&2; exit 1; }
export EMBYRELAY_INSTALLER_DIRECTORY
EMBYRELAY_INSTALLER_DIRECTORY=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
exec python3 - "$@" <<'PY'
import argparse
import hashlib
import shutil
import base64
import fcntl
import json
import os
from pathlib import Path
import platform
import re
import shlex
import socket
import subprocess
import sys
import time
import tempfile
import urllib.request
from urllib.parse import urlsplit


def ask(label, default=''):
    with open('/dev/tty', 'r') as tty, open('/dev/tty', 'w') as output:
        output.write(label + (f' [{default}]' if default else '') + ': ')
        output.flush()
        value = tty.readline()
        if not value:
            raise ValueError('没有交互输入，请通过命令参数提供配置')
        return value.strip() or default


def origin(value):
    u = urlsplit(value)
    if (u.scheme != 'https' or not u.hostname or u.username or u.password
            or u.path not in ('', '/') or u.query or u.fragment
            or any(c.isspace() for c in value)):
        raise ValueError('控制端地址必须是 HTTPS origin，例如 https://panel.example.com')
    _ = u.port
    return value.rstrip('/')


def domain(value):
    value = value.lower().rstrip('.')
    if len(value) > 253 or '.' not in value or not all(
        re.fullmatch(r'[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?', p) for p in value.split('.')
    ):
        raise ValueError('节点域名格式错误，请只填域名，不含协议、端口或路径')
    try:
        socket.inet_aton(value)
    except OSError:
        return value
    raise ValueError('请使用域名，而不是 IP 地址')


def write_private(path, text):
    temporary = path.with_name(path.name + '.tmp')
    with open(temporary, 'w', encoding='utf-8') as f:
        os.chmod(temporary, 0o600)
        f.write(text)
    os.replace(temporary, path)


def compose_text(config):
    # Compose interpolates dollar signs even inside JSON/YAML quoted strings.
    return json.dumps(config, ensure_ascii=False, indent=2).replace('$', '$$') + '\n'


def available_ports(mode):
    ports = [('127.0.0.1', 6689, socket.SOCK_STREAM)]
    for host, port, kind in ports:
        with socket.socket(socket.AF_INET, kind) as s:
            try:
                s.bind((host, port))
            except OSError as e:
                raise ValueError(f'端口 {port} 不可用；请先处理端口冲突：{e}') from e


def configuration(control, public_domain, name, token, key, mode):
    config = {
        'name': 'embyrelay-agent',
        'x-embyrelay-installer': {'version': 2, 'mode': mode, 'domain': public_domain},
        'services': {'agent': {
            'image': os.environ.get('RELAY_IMAGE', 'ecouus/embyrelay:latest'),
            'entrypoint': ['/usr/local/bin/embyrelay-agent'],
            'environment': {'RELAY_LISTEN': '0.0.0.0:6689', 'RELAY_CONTROL_URL': control,
                'RELAY_PUBLIC_URL': 'https://' + public_domain, 'RELAY_NODE_NAME': name,
                'RELAY_ENROLL_TOKEN': token, 'RELAY_SERVER_KEY': key,
                'RELAY_TRUSTED_PROXIES': '127.0.0.1/32'},
            'volumes': ['agent_data:/data'], 'ports': ['127.0.0.1:6689:6689'],
            'read_only': True, 'cap_drop': ['ALL'],
            'security_opt': ['no-new-privileges:true'], 'restart': 'unless-stopped',
            'stop_grace_period': '15s',
        }},
        'volumes': {'agent_data': {}},
    }
    config['services']['agent'].pop('ports')
    config['services']['agent']['network_mode'] = 'host'
    config['services']['agent']['environment']['RELAY_LISTEN'] = '127.0.0.1:6689'
    return config


def run(command, **kwargs):
    return subprocess.run(command, check=True, **kwargs)


NGINX_TEMPLATE = 'map $http_upgrade $embyrelay_connection {\n    default upgrade;\n    \'\'      close;\n}\n\nserver {\n    listen 80;\n    server_name relay.example.com;\n\n    access_log off;\n\n    # 使用 webroot 方式签发证书时，保留验证目录\n    location ^~ /.well-known/acme-challenge/ {\n        root /var/www/html;\n    }\n    location / {\n        return 301 https://relay.example.com$request_uri;\n    }\n}\n\nserver {\n    listen 443 ssl;\n    server_name relay.example.com;\n\n    ssl_certificate     /etc/letsencrypt/live/relay.example.com/fullchain.pem;\n    ssl_certificate_key /etc/letsencrypt/live/relay.example.com/privkey.pem;\n    ssl_protocols TLSv1.2 TLSv1.3;\n\n    # 播放 URL 含访问密钥，不记录访问日志\n    access_log off;\n    add_header Referrer-Policy "no-referrer" always;\n    add_header X-Content-Type-Options "nosniff" always;\n\n    location / {\n        # 保留完整的 /r/... 播放路径\n        proxy_pass http://127.0.0.1:6689;\n        proxy_http_version 1.1;\n\n        # 保留端口，确保后台同源校验正确\n        proxy_set_header Host              $http_host;\n        proxy_set_header X-Real-IP         $remote_addr;\n        proxy_set_header X-Forwarded-For   $remote_addr;\n        proxy_set_header X-Forwarded-Proto $scheme;\n\n        # WebSocket\n        proxy_set_header Upgrade    $http_upgrade;\n        proxy_set_header Connection $embyrelay_connection;\n\n        # 流式转发，不缓存视频响应\n        proxy_buffering off;\n        proxy_request_buffering off;\n        proxy_cache off;\n\n        proxy_connect_timeout 10s;\n        proxy_read_timeout 3600s;\n        proxy_send_timeout 3600s;\n    }\n}\n'

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
    result = subprocess.run(['python3', str(script), '--directory', str(directory), '--role', role] + (options or []))
    if result.returncode:
        raise ValueError('公共升级器未通过；请查看其错误和保留的恢复命令')
    if temporary:
        shutil.rmtree(temporary)


def main():
    parser = argparse.ArgumentParser(prog='install-agent.sh', description='EmbyRelay Agent 部署（Debian / Ubuntu，需 Docker Compose v2）')
    parser.add_argument('action', nargs='?', choices=['install', 'upgrade'], default='install')
    parser.add_argument('--version', help='upgrade 目标版本')
    parser.add_argument('--yes', action='store_true', help='upgrade 非交互确认')
    parser.add_argument('--control-url')
    parser.add_argument('--server-key')
    parser.add_argument('--token')
    parser.add_argument('--domain')
    parser.add_argument('--name')
    parser.add_argument('--mode', choices=['nginx'], default='nginx', help='宿主机 Nginx + Certbot（默认）')
    parser.add_argument('--directory', default='/opt/embyrelay-agent')
    args = parser.parse_args()
    if platform.system() != 'Linux' or os.geteuid() != 0:
        raise ValueError('请以 root 在 B 的 Debian / Ubuntu 服务器上执行')
    for command in [['docker', 'info'], ['docker', 'compose', 'version']]:
        result = subprocess.run(command, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if result.returncode:
            raise ValueError('需要可访问的 Docker Engine 和 Compose v2，请先安装并启动 Docker：https://docs.docker.com/engine/install/')
    os.umask(0o077)
    directory = Path(args.directory).resolve()
    existing_path = directory / 'docker-compose.yml'
    existing_registered = False
    if existing_path.is_file():
        try:
            existing = json.loads(existing_path.read_text())
            existing_registered = (existing.get('x-embyrelay-installer', {}).get('version') in (1, 2) and
                'agent' in existing.get('services', {}) and not existing['services']['agent'].get('environment', {}).get('RELAY_ENROLL_TOKEN'))
        except (ValueError, KeyError, TypeError):
            pass
    if args.action == 'upgrade' or existing_registered:
        options = (['--version', args.version] if args.version else []) + (['--yes'] if args.yes else [])
        shared_upgrade(directory, 'agent', options)
        return
    directory.mkdir(parents=True, exist_ok=True)
    with open(directory / '.install.lock', 'w') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise ValueError('此目录已有部署任务在运行')
        path = directory / 'docker-compose.yml'
        proxy_ready = False
        if path.exists():
            try:
                config = json.loads(path.read_text().replace('$$', '$'))
                metadata = config['x-embyrelay-installer']
                if metadata['version'] not in (1, 2):
                    raise ValueError('unsupported version')
            except (ValueError, KeyError, TypeError):
                raise ValueError('已有配置不是本脚本生成的配置；为保留现有部署，停止操作')
            if metadata.get('mode') != 'nginx':
                raise ValueError('检测到旧版容器反代部署，请先按部署手册迁移到宿主机 Nginx；配置与数据未改动')
            if metadata['version'] == 1:
                raise ValueError('检测到旧版部署。自动保留旧网络以免改动现有反代；请按部署文档迁移真实 IP 配置，再用 docker compose pull / up -d 更新。')
            print('检测到已有部署：保留节点配置、数据卷和身份，检查 HTTPS 反代并更新镜像。', flush=True)
            if config['services']['agent']['environment'].get('RELAY_ENROLL_TOKEN') and args.token:
                config['services']['agent']['environment']['RELAY_ENROLL_TOKEN'] = args.token
                write_private(path, compose_text(config))
        else:
            control = origin(args.control_url or ask('A 控制台 HTTPS 地址'))
            host = domain(args.domain or ask('B 的域名（先将 DNS 解析到本机）'))
            mode = 'nginx'
            name = args.name or ask('节点名称', socket.gethostname())
            key = args.server_key or ask('A 面板签名公钥')
            try:
                if len(base64.b64decode(key + '=' * (-len(key) % 4), validate=True)) != 32:
                    raise ValueError('length')
            except ValueError:
                raise ValueError('签名公钥必须是面板提供的 Base64 Ed25519 公钥')
            available_ports(mode)
            setup_nginx([], directory, path, configuration(control, host, name, "", key, mode))
            proxy_ready = True
            print('正在拉取镜像；完成后再输入一次性令牌，避免下载期间过期。', flush=True)
            subprocess.run(['docker', 'pull', os.environ.get('RELAY_IMAGE', 'ecouus/embyrelay:latest')], check=True)
            token = args.token or ask('A 面板一次性注册令牌（10 分钟有效）')
            if not token:
                raise ValueError('注册令牌不能为空')
            config = configuration(control, host, name, token, key, mode)
            write_private(path, compose_text(config))
        compose = ['docker', 'compose', '-f', str(path)]
        if not proxy_ready:
            setup_nginx(compose, directory, path, config)
        subprocess.run(compose + ['config', '--quiet'], check=True)
        subprocess.run(compose + ['pull'], check=True)
        subprocess.run(compose + ['up', '-d', '--force-recreate'], check=True)
        print('等待 Agent 注册并就绪（最多 90 秒）…', flush=True)
        def wait_healthy():
            for _ in range(30):
                result = subprocess.run(compose + ['exec', '-T', 'agent', 'wget', '-q', '-O', '/dev/null', 'http://127.0.0.1:6689/healthz'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                if result.returncode == 0:
                    return
                time.sleep(3)
            raise ValueError(f'Agent 尚未就绪，配置与数据已保留。查看日志：{shlex.join(compose)} logs --tail=100 agent。令牌过期时重新生成，并用 --token 新令牌重试。')
        wait_healthy()
        environment = config['services']['agent']['environment']
        if environment.get('RELAY_ENROLL_TOKEN'):
            environment['RELAY_ENROLL_TOKEN'] = ''
            write_private(path, compose_text(config))
            # Recreate to remove the consumed token from container metadata too.
            subprocess.run(compose + ['up', '-d', '--force-recreate'], check=True)
            wait_healthy()
        print('Agent 已注册并通过本机健康检查，一次性令牌已从配置清除。', flush=True)
        print('Nginx HTTPS 反代已配置，Certbot 自动续签已启用；请确认公网 DNS 和 80/443 可达。')
        print('回 A 面板确认节点在线，再在代理路由中选择此节点。')
        print(f'配置目录：{directory}\n日志：{shlex.join(compose)} logs --tail=100 -f\n更新：重新运行脚本并使用相同 --directory。请勿删除数据卷或执行 down -v。')


if __name__ == '__main__':
    try:
        main()
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        print(f'部署未完成：{error}', file=sys.stderr)
        sys.exit(1)
PY
