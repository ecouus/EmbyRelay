#!/usr/bin/env python3
"""Compose upgrade transaction. Configuration is data, never executable code."""
import argparse
import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import secrets
import shlex
import shutil
import signal
import sqlite3
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.request
from urllib.parse import urlsplit, unquote

REPOSITORY = 'ecouus/embyrelay'
VERSION = re.compile(r'v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?')


class UpgradeError(Exception):
    def __init__(self, message, code=1):
        super().__init__(message)
        self.code = code


def fail(message, code=1):
    raise UpgradeError(message, code)


def version_key(value):
    m = VERSION.fullmatch(value or '')
    if not m:
        fail('版本必须为 X.Y.Z 或 X.Y.Z-prerelease', 2)
    pre = m[4]
    if pre and any(x.isdigit() and len(x) > 1 and x[0] == '0' for x in pre.split('.')):
        fail('预发布数字标识不能含前导零', 2)
    return tuple(map(int, m.group(1, 2, 3))) + (pre is None, tuple(
        (0, int(x)) if x.isdigit() else (1, x) for x in (pre or '').split('.') if x))


def private_write(path, data):
    path = Path(path)
    fd, temporary = tempfile.mkstemp(prefix='.' + path.name, dir=path.parent)
    try:
        with os.fdopen(fd, 'wb') as output:
            output.write(data if isinstance(data, bytes) else data.encode())
            output.flush()
            os.fsync(output.fileno())
        if path.exists():
            old = path.stat()
            os.chown(temporary, old.st_uid, old.st_gid)
        os.replace(temporary, path)
    finally:
        Path(temporary).unlink(missing_ok=True)


def save_json(path, data):
    private_write(path, json.dumps(data, ensure_ascii=False, indent=2) + '\n')


def read_config(path):
    # JSON is also supported without an additional parser, as in both installers.
    text = Path(path).read_text()
    try:
        def unique(pairs):
            result = {}
            for key, value in pairs:
                if key in result:
                    fail('配置包含重复字段，拒绝猜测', 2)
                result[key] = value
            return result
        result = json.loads(text, object_pairs_hook=unique)
    except json.JSONDecodeError:
        try:
            import yaml
        except ImportError:
            fail('YAML 部署需要 python3-yaml，请安装后重试', 2)
        class SafeUnique(yaml.SafeLoader):
            pass
        def mapping(loader, node, deep=False):
            keys = [n.value for n, _ in node.value if n.tag != 'tag:yaml.org,2002:merge']
            if len(keys) != len(set(keys)):
                fail('YAML 配置包含重复字段', 2)
            loader.flatten_mapping(node)
            return yaml.SafeLoader.construct_mapping(loader, node, deep)
        SafeUnique.add_constructor('tag:yaml.org,2002:map', mapping)
        try:
            result = yaml.load(text, Loader=SafeUnique)
        except yaml.YAMLError:
            fail('配置不是受支持的安全 YAML', 2)
    if not isinstance(result, dict) or not isinstance(result.get('services'), dict):
        fail('Compose 配置缺少 services', 2)
    if 'include' in result or any('extends' in s for s in result['services'].values()):
        fail('暂不支持 include/extends；请使用显式多文件 Compose 部署', 2)
    return result


def encode_config(config):
    # Preserve interpolation strings: only the selected service image is changed.
    return json.dumps(config, ensure_ascii=False, indent=2) + '\n'


def request_json(url):
    try:
        with urllib.request.urlopen(url, timeout=20) as response:
            return json.load(response)
    except Exception:
        fail('版本查询失败（网络、认证或返回内容无效）；未进行升级', 3)


def latest_version(prerelease=False):
    url = 'https://hub.docker.com/v2/repositories/' + REPOSITORY + '/tags?page_size=100'
    versions = []
    for _ in range(100):
        result = request_json(url)
        if not isinstance(result, dict) or not isinstance(result.get('results'), list):
            fail('镜像仓库版本响应无效', 3)
        for tag in result['results']:
            name = tag.get('name', '')
            if VERSION.fullmatch(name):
                key = version_key(name)
                if prerelease or key[3]:
                    versions.append(name.lstrip('v'))
        url = result.get('next')
        if not url:
            break
        if not url.startswith('https://hub.docker.com/v2/repositories/' + REPOSITORY + '/tags?'):
            fail('镜像仓库返回不可信分页地址', 3)
    else:
        fail('版本列表过长，无法确认最新版本，请指定 --version', 3)
    if not versions:
        fail('未发现有效的正式镜像版本', 3)
    return max(versions, key=version_key)


class Docker:
    def run(self, command, *, stage='Docker 操作', **kwargs):
        # Never print argv or Docker stderr: either can contain credentials.
        kwargs.setdefault('stdout', subprocess.PIPE)
        kwargs.setdefault('stderr', subprocess.PIPE)
        try:
            result = subprocess.run(command, **kwargs)
        except OSError:
            fail(stage + '失败，请检查工具和权限', 3)
        if result.returncode:
            fail(stage + '失败，请检查 Docker、认证、网络或部署配置', 3)
        return result.stdout

    def json(self, command, **kwargs):
        try:
            return json.loads(self.run(command, **kwargs))
        except (ValueError, TypeError):
            fail('Docker 返回内容无效', 3)

    def inspect(self, identifier):
        return self.json(['docker', 'inspect', identifier])[0]

    def containers(self):
        ids = self.run(['docker', 'ps', '-aq'], text=True).split()
        return self.json(['docker', 'inspect'] + ids) if ids else []


def environment(service):
    value = service.get('environment') or {}
    if isinstance(value, dict):
        return {k: str(v) for k, v in value.items() if v is not None}
    return dict(x.split('=', 1) for x in value if '=' in x)


def role_of(service):
    image = service.get('image', '')
    # Image identity and executable must both belong to the project.
    if not re.fullmatch(r'(?:docker\.io/)?ecouus/embyrelay(?::[^\s/@]+)?(?:@sha256:[0-9a-f]{64})?', image):
        return None
    entry = service.get('entrypoint')
    if entry is None:
        role = 'control'
    elif entry in (['/usr/local/bin/embyrelay'], '/usr/local/bin/embyrelay'):
        role = 'control'
    elif entry in (['/usr/local/bin/embyrelay-agent'], '/usr/local/bin/embyrelay-agent'):
        role = 'agent'
    else:
        fail('不支持自定义应用 entrypoint', 2)
    if service.get('command'):
        fail('不支持自定义应用 command', 2)
    if service.get('scale', 1) != 1 or service.get('deploy', {}).get('replicas', 1) != 1:
        fail('升级仅支持每个服务一个实例', 2)
    return role


class Deployment:
    def __init__(self, docker, files, project=None, service=None, container=None):
        self.docker = docker
        self.files = [Path(f).resolve() for f in files]
        self.directory = self.files[0].parent
        self.raw = [read_config(f) for f in self.files]
        self.compose = ['docker', 'compose', '--project-directory', str(self.directory)]
        if project:
            self.compose += ['--project-name', project]
        for f in self.files:
            self.compose += ['-f', str(f)]
        self.model = docker.json(self.compose + ['config', '--format', 'json'], stage='解析 Compose 配置')
        self.project = self.model['name']
        if project and project != self.project:
            fail('Compose 项目与容器信息冲突', 2)
        candidates = [(n, role_of(s)) for n, s in self.model['services'].items()]
        candidates = [(n, r) for n, r in candidates if r and (not service or n == service)]
        if len(candidates) != 1:
            fail('配置存在多个应用角色或未找到目标服务；请指定 --instance PROJECT/SERVICE', 2)
        self.service, self.role = candidates[0]
        self.app = self.model['services'][self.service]
        self.env = environment(self.app)
        self.container = container
        self.db = self.env.get('RELAY_DB_DRIVER', 'sqlite') if self.role == 'control' else 'sqlite'
        if self.db not in ('sqlite', 'postgres'):
            fail('不支持该数据库类型', 2)
        if self.app.get('network_mode') not in (None, 'host'):
            fail('仅支持 Compose bridge 或 host 网络', 2)
        if self.env.get('RELAY_TLS_CERT') or self.env.get('RELAY_TLS_KEY'):
            fail('暂不支持应用内原生 TLS；支持宿主机/Compose 反代 HTTPS', 2)
        self.data_dir = self.env.get('RELAY_DATA_DIR', '/data')
        mounts = self.app.get('volumes', [])
        data = [m for m in mounts if m.get('target') == self.data_dir]
        if len(data) != 1 or data[0].get('type') not in ('bind', 'volume') or data[0].get('read_only'):
            fail('需要唯一、可写、持久的应用数据挂载', 2)
        self.mount = dict(data[0])
        if self.mount['type'] == 'volume':
            self.mount['source'] = self.model['volumes'][self.mount['source']]['name']
        for m in mounts:
            if m is data[0]:
                continue
            if not m.get('read_only') or m['target'].startswith(self.data_dir.rstrip('/') + '/'):
                fail('不支持额外可写挂载或数据目录子挂载', 2)
        if container:
            labels = container['Config'].get('Labels') or {}
            if labels.get('com.docker.compose.project') != self.project or labels.get('com.docker.compose.service') != self.service:
                fail('运行容器与目标 Compose 不一致', 2)
            actual = [m for m in container['Mounts'] if m['Destination'] == self.data_dir]
            source = self.mount['source']
            if len(actual) != 1 or not actual[0].get('RW', True) or actual[0]['Type'] != self.mount['type'] or (actual[0].get('Name') if self.mount['type'] == 'volume' else actual[0]['Source']) != source:
                fail('实际数据挂载与 Compose 不一致，拒绝升级', 2)
            if role_of({'image': container['Config']['Image'], 'entrypoint': container['Config'].get('Entrypoint')}) != self.role:
                fail('容器角色与配置不一致', 2)
            actual_env = environment({'environment': container['Config'].get('Env', [])})
            for key in ('RELAY_DB_DRIVER', 'RELAY_DATA_DIR', 'RELAY_DATABASE_URL', 'RELAY_DATABASE_URL_FILE', 'RELAY_CONTROL_URL'):
                default = 'sqlite' if key == 'RELAY_DB_DRIVER' else '/data' if key == 'RELAY_DATA_DIR' else ''
                if actual_env.get(key, default) != self.env.get(key, default):
                    fail('数据库/身份配置与容器存在未应用的差异，先处理配置漂移', 2)
        if self.mount['type'] == 'volume':
            info = docker.json(['docker', 'volume', 'inspect', self.mount['source']])[0]
            self.data_source = Path(info['Mountpoint'])
            if not container and (info.get('Labels') or {}).get('com.docker.compose.project') != self.project:
                fail('无容器时无法确认数据卷所属项目', 2)
        else:
            self.data_source = Path(self.mount['source']).resolve()
            if not self.data_source.is_dir():
                fail('数据绑定目录不存在；禁止创建空数据目录', 2)
        if self.directory == self.data_source or self.data_source in self.directory.parents:
            fail('部署/备份目录不能位于应用数据挂载中', 2)
        self.metadata = next((r.get('x-embyrelay-installer', {}) for r in self.raw if 'x-embyrelay-installer' in r), {})
        pending = self.directory / 'docker-compose.migrate.yml'
        if pending.exists() and read_config(pending).get('x-embyrelay-installer', {}).get('migration_started'):
            fail('检测到未收尾的数据库迁移，先完成迁移', 2)
        self.pg = None
        if self.db == 'postgres':
            self.setup_postgres()

    def command(self, *args, **kwargs):
        return self.docker.run(self.compose + list(args), **kwargs)

    def mount_args(self, writable=False):
        m = self.mount
        return ['--mount', 'type=' + m['type'] + ',src=' + m['source'] + ',dst=/snapshot' + ('' if writable else ',readonly')]

    def helper(self, image, script, *, writable=False, **kwargs):
        return self.docker.run(['docker', 'run', '--rm', '--network', 'none', '--read-only', '--user', '0'] +
            (['-i'] if kwargs.get('stdin') is not None else []) + self.mount_args(writable) + ['--entrypoint', 'sh', image, '-c', script], **kwargs)

    def setup_postgres(self):
        url = self.env.get('RELAY_DATABASE_URL', '')
        if self.env.get('RELAY_DATABASE_URL_FILE'):
            target = self.env['RELAY_DATABASE_URL_FILE']
            sources = [m['source'] for m in self.app.get('volumes', []) if m['target'] == target and m['type'] == 'bind' and m.get('read_only')]
            if len(sources) != 1:
                sources = [self.model['secrets'][s['source']]['file'] for s in self.app.get('secrets', []) if s.get('target', s['source']) in (target, Path(target).name)]
            if len(sources) != 1:
                fail('无法确认 PostgreSQL 凭据文件挂载', 2)
            url = Path(sources[0]).read_text().strip()
        try:
            u = urlsplit(url)
            user, db = unquote(u.username or ''), unquote(u.path.lstrip('/'))
            port = u.port or 5432
        except ValueError:
            fail('PostgreSQL URL 无效', 2)
        if u.scheme not in ('postgres', 'postgresql') or not re.fullmatch(r'[A-Za-z_][A-Za-z_0-9]*', user) or not re.fullmatch(r'[A-Za-z_][A-Za-z_0-9]*', db):
            fail('PostgreSQL 需要标准数据库用户与数据库名', 2)
        if any(x in u.query for x in ('search_path', 'options', 'schema')):
            fail('暂不支持自定义 PostgreSQL schema/search_path', 2)
        matches = []
        for name, svc in self.model['services'].items():
            env = environment(svc)
            if not re.fullmatch(r'(?:docker\.io/library/)?postgres:17(?:\.[0-9]+)?(?:-alpine(?:[0-9.]+)?)?', svc.get('image', '')):
                continue
            bridge = u.hostname == name and port == 5432
            local = u.hostname in ('127.0.0.1', 'localhost') and any(int(p.get('published', 0)) == port and p['target'] == 5432 and p.get('host_ip') in ('127.0.0.1', '::1') for p in svc.get('ports', []))
            if (bridge or local) and env.get('POSTGRES_USER', 'postgres') == user and env.get('POSTGRES_DB', env.get('POSTGRES_USER', 'postgres')) == db:
                matches.append(name)
        if len(matches) != 1:
            fail('仅支持同一 Compose 项目中专用 PostgreSQL 17；外部/共享数据库需人工备份升级', 2)
        self.pg = matches[0], user, db
        for name, svc in self.model['services'].items():
            if name != self.service and name != self.pg[0] and self.pg[0] in svc.get('depends_on', {}):
                fail('PostgreSQL 被其他服务依赖，不能自动回滚共享数据库', 2)

    def postgres(self, program, *args, **kwargs):
        service, user, db = self.pg
        connection = [] if program == 'pg_restore' else ['-U', user, '-d', db]
        return self.command('exec', '-T', service, program, *connection, *args, **kwargs)

    def public_url(self):
        value = self.env.get('RELAY_PUBLIC_URL') or self.app.get('labels', {}).get('io.embyrelay.public-url')
        if not value and self.metadata.get('domain'):
            value = 'https://' + self.metadata['domain']
        return value


def original_files(files, project, service):
    last = Path(files[-1])
    if last.name == 'runtime-override.json' and (last.parent / 'manifest.json').is_file():
        manifest = json.loads((last.parent / 'manifest.json').read_text())
        if (manifest.get('format') == 1 and manifest.get('project') == project and
                manifest.get('service') == service and manifest.get('files') == files[:-1]):
            return files[:-1]
    return files


def discover(docker, args, allow_missing=False):
    containers = docker.containers()
    groups = {}
    instance = args.instance.split('/', 1) if args.instance else None
    if instance and (len(instance) != 2 or not all(instance)):
        fail('--instance 格式为 PROJECT/SERVICE', 2)
    for c in containers:
        labels = c['Config'].get('Labels') or {}
        files = labels.get('com.docker.compose.project.config_files', '').split(',')
        project, service = labels.get('com.docker.compose.project'), labels.get('com.docker.compose.service')
        if not project or not service or not files[0]:
            continue
        if not re.match(r'(?:docker\.io/)?ecouus/embyrelay(?=[:@]|$)', c['Config']['Image']):
            continue
        if instance and [project, service] != instance:
            continue
        if args.directory and Path(labels.get('com.docker.compose.project.working_dir', '')).resolve() != Path(args.directory).resolve():
            continue
        key = project, service
        if key in groups:
            fail('同一服务有多个容器；请处理副本歧义', 2)
        groups[key] = (original_files(files, project, service), project, service, c)
    paths = [Path(args.directory).resolve()] if args.directory else [Path.cwd(), Path('/opt/embyrelay'), Path('/opt/embyrelay-agent')]
    errors = []
    for directory in dict.fromkeys(paths):
        candidates = [directory / n for n in ('compose.yaml', 'compose.yml', 'docker-compose.yml', 'docker-compose.yaml') if (directory / n).is_file()]
        if len(candidates) > 1:
            errors.append('目录存在多份基础 Compose 文件；请用 --file 明确指定')
            continue
        if not candidates:
            continue
        files = candidates
        for n in ('compose.override.yaml', 'compose.override.yml', 'docker-compose.override.yml', 'docker-compose.override.yaml'):
            if (directory / n).is_file():
                files = files + [directory / n]
        if any(Path(v[0][0]).resolve() == candidates[0].resolve() for v in groups.values()):
            continue
        raw = read_config(candidates[0])
        for service, svc in raw['services'].items():
            if role_of(svc):
                project = instance[0] if instance else raw.get('name')
                if instance and service != instance[1]:
                    continue
                key = str(directory), service
                groups[key] = ([str(f) for f in files], project, service, None)
    if args.file:
        groups = {('explicit', ''): (args.file, instance[0] if instance else None, instance[1] if instance else None, None)}
        # Match actual containers after Compose resolves project/service below.
    deployments = []
    for files, project, service, container in groups.values():
        try:
            dep = Deployment(docker, files, project, service, container)
            if not container:
                actual = [c for c in containers if (c['Config'].get('Labels') or {}).get('com.docker.compose.project') == dep.project and (c['Config'].get('Labels') or {}).get('com.docker.compose.service') == dep.service]
                if len(actual) > 1:
                    fail('发现多个应用容器', 2)
                if actual:
                    dep = Deployment(docker, files, dep.project, dep.service, actual[0])
            if not args.role or dep.role == args.role:
                deployments.append(dep)
        except UpgradeError as e:
            errors.append(str(e))
    if errors:
        fail('部署识别失败：' + '；'.join(errors), 2)
    if not deployments:
        if allow_missing:
            return None
        fail('未发现受支持的 Compose 部署，请指定 --directory 或 --file', 2)
    if allow_missing:
        return deployments[0]  # Presence probe only; actual selection happens in the upgrade call.
    if len(deployments) > 1:
        if args.yes or not sys.stdin.isatty():
            fail('发现多个部署；非交互模式必须指定 --instance PROJECT/SERVICE', 2)
        for i, dep in enumerate(deployments, 1):
            print(f'{i}. {dep.role}  {dep.project}/{dep.service}  {dep.directory}')
        try:
            selected = int(input('选择实例编号：'))
            if not 1 <= selected <= len(deployments):
                raise ValueError()
        except ValueError:
            fail('无效的选择', 2)
        return deployments[selected - 1]
    return deployments[0]


def file_hash(path):
    result = hashlib.sha256()
    with Path(path).open('rb') as reader:
        for chunk in iter(lambda: reader.read(1024 * 1024), b''):
            result.update(chunk)
    return result.hexdigest()


def validate_archive(path):
    # Same invariant as the installer's restore: no traversal, links, or devices.
    with tarfile.open(path, 'r:gz') as archive:
        for member in archive.getmembers():
            name = Path(member.name)
            if name.is_absolute() or '..' in name.parts or not (member.isfile() or member.isdir()):
                fail('数据备份包含不安全路径或特殊文件', 4)


def archive_state(path, role, database):
    validate_archive(path)
    state = {'hashes': {}}
    with tarfile.open(path, 'r:gz') as archive:
        members = {str(Path(m.name)): m for m in archive.getmembers()}
        for name in ('master.key', 'identity.json'):
            if name in members:
                raw = archive.extractfile(members[name]).read()
                state['hashes'][name] = hashlib.sha256(raw).hexdigest()
        if role == 'agent' and 'identity.json' not in state['hashes']:
            fail('Agent 数据缺少已注册身份，禁止重新注册', 4)
        if database == 'sqlite':
            name = 'agent.db' if role == 'agent' else 'embyrelay.db'
            if name not in members or members[name].size == 0:
                fail('原数据库不存在/为空，禁止初始化空账号库', 4)
            with tempfile.TemporaryDirectory() as directory:
                for suffix in ('', '-wal', '-shm'):
                    filename = name + suffix
                    if filename in members:
                        with archive.extractfile(members[filename]) as reader, (Path(directory) / filename).open('wb') as output:
                            shutil.copyfileobj(reader, output)
                try:
                    with contextlib.closing(sqlite3.connect(str(Path(directory) / name))) as connection:
                        if connection.execute('PRAGMA quick_check').fetchone()[0] != 'ok':
                            fail('SQLite 完整性校验失败', 4)
                        state['schema'] = connection.execute('SELECT MAX(version) FROM schema_migrations').fetchone()[0]
                except sqlite3.DatabaseError:
                    fail('无法读取原 SQLite 数据库 schema', 4)
    return state


def health(docker, identifier, listen, database, version=None, role=None):
    try:
        port = int(listen.rsplit(':', 1)[1])
        if not 1 <= port <= 65535:
            raise ValueError()
    except (ValueError, IndexError):
        fail('RELAY_LISTEN 地址无效', 2)
    value = docker.json(['docker', 'exec', identifier, 'wget', '-q', '-O', '-',
                         f'http://127.0.0.1:{port}/healthz'], stage='本机健康检查')
    if value.get('status') != 'ok' or value.get('database', 'sqlite') != database:
        fail('健康接口或数据库类型校验失败', 5)
    if version and value.get('version') != version:
        fail('实际软件版本与目标不一致', 5)
    if role and bool(value.get('agent')) != (role == 'agent'):
        fail('健康接口角色与目标不一致', 5)
    version_key(value.get('version', ''))
    return value


def probe_image(docker, image):
    name = 'embyrelay-upgrade-probe-' + secrets.token_hex(6)
    created = False
    probe_data = tempfile.TemporaryDirectory(prefix='embyrelay-image-probe-')
    os.chown(probe_data.name, 10001, 10001)
    try:
        docker.run(['docker', 'run', '-d', '--name', name, '--network', 'none', '--read-only',
                    '--mount', 'type=bind,src=' + probe_data.name + ',dst=/data', '-e', 'RELAY_ADMIN_USER=probe',
                    '-e', 'RELAY_ADMIN_PASSWORD=' + secrets.token_urlsafe(24), '-e', 'RELAY_ALLOW_INSECURE=true',
                    '-e', 'RELAY_LISTEN=127.0.0.1:6689', '--entrypoint', '/usr/local/bin/embyrelay', image], stage='隔离验证镜像内容')
        created = True
        for _ in range(30):
            try:
                result = health(docker, name, '127.0.0.1:6689', 'sqlite', role='control')
                break
            except UpgradeError:
                time.sleep(.2)
        else:
            fail('目标镜像无法在隔离环境启动', 3)
        docker.run(['docker', 'stop', '-t', '15', name])
        with contextlib.closing(sqlite3.connect(str(Path(probe_data.name) / 'embyrelay.db'))) as connection:
            result['schema'] = connection.execute('SELECT MAX(version) FROM schema_migrations').fetchone()[0]
        # Both executables must be present; the Agent must not run or enroll here.
        docker.run(['docker', 'run', '--rm', '--network', 'none', '--read-only', '--entrypoint', 'sh', image,
                    '-c', 'test -x /usr/local/bin/embyrelay-agent && test -x /usr/local/bin/embyrelay'])
        return result
    finally:
        if created:
            docker.run(['docker', 'rm', '-f', name], stage='清理隔离镜像验证容器')
        probe_data.cleanup()


def check_public(url, expected, role, database):
    if not url:
        print('未发现明确的 HTTPS 入口；请人工验证公网反代。可通过 --public-url 指定。')
        return
    u = urlsplit(url)
    if u.scheme != 'https' or not u.hostname or u.username or u.password or u.query or u.fragment or u.path not in ('', '/'):
        fail('公网入口必须是无凭据的 HTTPS origin', 2)
    # Redirects cannot silently validate another instance or an HTTP endpoint.
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, *args, **kwargs):
            return None
    opener = urllib.request.build_opener(NoRedirect())
    try:
        with opener.open(url.rstrip('/') + '/healthz', timeout=10) as response:
            value = json.load(response)
        if (value.get('status') != 'ok' or value.get('version') != expected or
                bool(value.get('agent')) != (role == 'agent') or value.get('database', 'sqlite') != database):
            raise ValueError()
    except Exception:
        fail('HTTPS 入口的证书、角色、版本或健康校验失败', 5)


class Transaction:
    def __init__(self, dep, timeout=90):
        self.dep, self.docker, self.timeout = dep, dep.docker, timeout
        self.backup = None
        self.state = {}
        self.stopped = False
        self.mutated = False
        self.backup_ready = False
        self.state_path = dep.directory / ('.upgrade-' + dep.service + '.json')

    def record(self, phase):
        self.state['phase'] = phase
        self.state['mutated'] = self.mutated or self.state.get('mutated', False)
        save_json(self.state_path, self.state)
        if self.backup:
            save_json(self.backup / 'manifest.json', self.state)

    def snapshot_configs(self):
        dep = self.dep
        files = set(dep.files)
        if (dep.directory / '.env').exists():
            files.add(dep.directory / '.env')
        for config, root in zip(dep.raw, dep.files):
            for service in config['services'].values():
                for item in service.get('env_file', []) if isinstance(service.get('env_file', []), list) else [service['env_file']]:
                    item = item.get('path') if isinstance(item, dict) else item
                    files.add((root.parent / item).resolve())
        for secret in dep.model.get('secrets', {}).values():
            if not secret.get('file'):
                fail('暂不支持 external/environment Compose secrets', 2)
            files.add(Path(secret['file']))
        for mount in dep.app.get('volumes', []):
            if mount.get('read_only') and mount['type'] == 'bind':
                files.add(Path(mount['source']))
        result = []
        for i, path in enumerate(sorted(files)):
            if not path.is_file():
                fail('配置/凭据需要普通文件；不支持目录形式的配置挂载', 2)
            stat = path.stat()
            saved = 'config-' + str(i)
            data = path.read_bytes()
            private_write(self.backup / saved, data)
            result.append({'path': str(path), 'saved': saved, 'mode': stat.st_mode & 0o777,
                           'uid': stat.st_uid, 'gid': stat.st_gid, 'sha256': hashlib.sha256(data).hexdigest()})
        return result

    def ensure_exclusive_data(self):
        dep = self.dep
        for c in self.docker.containers():
            if not c['State'].get('Running') or dep.container and c['Id'] == dep.container['Id']:
                continue
            for mount in c.get('Mounts', []):
                if mount.get('RW'):
                    source = Path(mount['Source']).resolve()
                    if source == dep.data_source or source in dep.data_source.parents or dep.data_source in source.parents:
                        fail('数据挂载仍被其他运行容器写入，不能一致性备份', 4)

    def disk_check(self):
        dep = self.dep
        size = sum(f.stat().st_size for f in dep.data_source.rglob('*') if f.is_file())
        if dep.pg:
            size += int(dep.postgres('psql', '-At', '-c', 'SELECT pg_database_size(current_database())', text=True).strip())
        required = max(64 * 1024 * 1024, size * 2)
        if shutil.disk_usage(dep.directory).free < required or shutil.disk_usage(dep.data_source).free < max(size, 16 * 1024 * 1024):
            fail('备份或数据文件系统空间不足，需要至少两倍数据大小的备份空间', 4)

    def save_data(self, destination):
        with destination.open('wb') as output:
            self.dep.helper(self.state['old_image'], 'exec tar czf - -C /snapshot .',
                            stdout=output, stage='完整数据备份')
        destination.chmod(0o600)
        state = archive_state(destination, self.dep.role, self.dep.db)
        if 'master.key' not in state['hashes'] and not self.dep.env.get('RELAY_MASTER_KEY'):
            fail('数据缺少主密钥，禁止启动新的密钥', 4)
        return state

    def wait(self, version, image):
        deadline = time.monotonic() + self.timeout
        while time.monotonic() < deadline:
            try:
                ids = self.dep.command('ps', '-a', '-q', self.dep.service, text=True).split()
                if len(ids) != 1:
                    fail('目标容器并非唯一', 5)
                current = self.docker.inspect(ids[0])
                if current['Image'] != image:
                    fail('实际运行镜像 ID 与预期不一致', 5)
                health(self.docker, ids[0], self.dep.env.get('RELAY_LISTEN', '0.0.0.0:6689'), self.dep.db, version, self.dep.role)
                return ids[0]
            except UpgradeError:
                time.sleep(.5)
        fail('目标实例健康/数据库/版本/镜像验收超时', 5)

    def start_image(self, image):
        override = self.backup / 'runtime-override.json'
        local_pin = REPOSITORY + ':upgrade-local-' + image.removeprefix('sha256:')
        self.docker.run(['docker', 'image', 'tag', image, local_pin], stage='固定已捕获的本地镜像')
        save_json(override, {'services': {self.dep.service: {'image': local_pin, 'pull_policy': 'never'}}})
        self.docker.run(self.dep.compose + ['-f', str(override), 'up', '-d', '--no-deps', '--no-build',
                                          '--force-recreate', '--pull', 'never', self.dep.service], stage='重建目标应用')

    def cancel_unmodified(self):
        dep = self.dep
        for item in self.state['configs']:
            if file_hash(item['path']) != item['sha256']:
                fail('未完成备份且原配置已变化，不能自动恢复', 6)
        ids = dep.command('ps', '-a', '-q', dep.service, text=True).split()
        if len(ids) != 1 or self.docker.inspect(ids[0])['Image'] != self.state['old_image']:
            fail('未完成备份且原容器/镜像不再匹配，需人工检查', 6)
        name = 'agent.db' if dep.role == 'agent' else 'embyrelay.db'
        if not (dep.data_source / name).is_file() and dep.db == 'sqlite':
            fail('原数据库不存在，禁止初始化', 6)
        if self.state['was_running']:
            dep.command('start', dep.service)
            self.wait(self.state['old_version'], self.state['old_image'])
        else:
            dep.command('stop', '-t', '20', dep.service)
        self.record('cancelled')
        print('升级未曾修改配置/数据；原实例运行状态已恢复，部分备份保留。')

    def restore(self):
        if not self.state.get('data_sha256'):
            if self.state.get('mutated', True):
                fail('数据已可能修改但缺少完整备份，需人工恢复', 6)
            self.cancel_unmodified()
            return
        dep = self.dep
        self.record('rolling-back')
        if self.docker.json(['docker', 'image', 'inspect', self.state['old_image']])[0]['Id'] != self.state['old_image']:
            fail('捕获的旧镜像 ID 已不可用，禁止按旧标签拉取', 6)
        dep.command('stop', '-t', '20', dep.service)
        self.ensure_exclusive_data()
        validate_archive(self.backup / 'data.tar.gz')
        if file_hash(self.backup / 'data.tar.gz') != self.state['data_sha256']:
            fail('数据备份哈希不匹配，应用保持停止', 6)
        if dep.pg and file_hash(self.backup / 'database.dump') != self.state.get('dump_sha256'):
            fail('PostgreSQL 备份哈希不匹配', 6)
        for item in self.state['configs']:
            if file_hash(self.backup / item['saved']) != item['sha256']:
                fail('原配置备份哈希不匹配，应用保持停止', 6)
        for item in self.state['configs']:
            data = (self.backup / item['saved']).read_bytes()
            if hashlib.sha256(data).hexdigest() != item['sha256']:
                fail('原配置备份哈希不匹配，应用保持停止', 6)
            path = Path(item['path'])
            private_write(path, data)
            os.chmod(path, item['mode'])
            os.chown(path, item['uid'], item['gid'])
        if dep.pg:
            if file_hash(self.backup / 'database.dump') != self.state.get('dump_sha256'):
                fail('PostgreSQL 备份哈希不匹配', 6)
            with (self.backup / 'database.dump').open('rb') as reader, (self.backup / 'restore.sql').open('wb') as output:
                dep.postgres('pg_restore', '--no-owner', '--no-acl', '-f', '-', stdin=reader, stdout=output, stage='生成 PostgreSQL 恢复事务')
            sql_path = self.backup / 'restore.sql'
            # Recreate the application's public schema so new-version-only tables also disappear.
            prepared_sql = self.backup / 'restore-prepared.sql'
            with sql_path.open('rb') as reader, prepared_sql.open('wb') as output:
                output.write(b'DROP SCHEMA public CASCADE; CREATE SCHEMA public;\n')
                for line in reader:
                    if line.rstrip(b'\r\n') != b'CREATE SCHEMA public;':
                        output.write(line)
            with prepared_sql.open('rb') as reader:
                dep.postgres('psql', '--single-transaction', '-v', 'ON_ERROR_STOP=1', stdin=reader, stage='单事务恢复 PostgreSQL')
        data = self.backup / 'data.tar.gz'
        validate_archive(data)
        if file_hash(data) != self.state['data_sha256']:
            fail('数据备份哈希不匹配，应用保持停止', 6)
        with data.open('rb') as reader:
            dep.helper(self.state['old_image'], 'set -eu; rm -rf /snapshot/* /snapshot/.[!.]* /snapshot/..?*; exec tar xzf - -C /snapshot',
                       writable=True, stdin=reader, stage='恢复完整数据和身份')
        self.start_image(self.state['old_image'])
        self.wait(self.state['old_version'], self.state['old_image'])
        self.verify_identity()
        if dep.db == 'sqlite':
            self.verify_sqlite(self.state['data_state']['schema'])
        elif int(dep.postgres('psql', '-At', '-c', 'SELECT MAX(version) FROM schema_migrations', text=True).strip()) != self.state['data_state']['schema']:
            fail('恢复后的 PostgreSQL schema 不匹配', 6)
        check_public(self.state.get('public_url'), self.state['old_version'], dep.role, dep.db)
        if not self.state['was_running']:
            dep.command('stop', '-t', '20', dep.service)
        self.record('rolled-back')
        print('已恢复旧配置、完整数据、原镜像和原运行状态；备份保留：' + str(self.backup))

    def verify_sqlite(self, expected):
        name = 'agent.db' if self.dep.role == 'agent' else 'embyrelay.db'
        with tempfile.TemporaryDirectory() as directory:
            # Online backup API obtains a consistent snapshot including WAL.
            with contextlib.closing(sqlite3.connect((self.dep.data_source / name).as_uri() + '?mode=ro', uri=True)) as source, contextlib.closing(sqlite3.connect(directory + '/snapshot.db')) as target:
                source.backup(target)
                if (target.execute('PRAGMA quick_check').fetchone()[0] != 'ok' or
                        target.execute('SELECT MAX(version) FROM schema_migrations').fetchone()[0] != expected):
                    fail('SQLite 数据库完整性/schema 验收失败', 5)

    def verify_identity(self):
        # Hash only identity/key files; never print their content.
        for filename, expected in self.state['data_state']['hashes'].items():
            result = self.dep.helper(self.state['old_image'], 'sha256sum /snapshot/' + filename, text=True)
            if result.split()[0] != expected:
                fail('主密钥或 Agent 身份发生改变', 5)

    def execute(self, args):
        dep = self.dep
        if self.state_path.exists():
            previous = json.loads(self.state_path.read_text())
            if previous.get('phase') not in ('complete', 'rolled-back', 'cancelled'):
                fail('上次事务未结束；先用 --restore ' + previous.get('backup', '备份目录') + ' 恢复', 6)
        if dep.container:
            old_image = dep.container['Image']
            running = dep.container['State'].get('Running', False)
        else:
            saved = json.loads(self.state_path.read_text()) if self.state_path.exists() else {}
            old_image = args.old_image or (saved.get('new_image') if saved.get('phase') == 'complete' else saved.get('old_image') if saved.get('phase') == 'rolled-back' else None)
            if not old_image or not re.fullmatch(r'sha256:[0-9a-f]{64}', old_image):
                fail('容器缺失时必须提供捕获的实际 --old-image sha256:...；禁止按可能变化的旧标签猜测', 2)
            running = False
        old = probe_image(self.docker, old_image)
        if running:
            current = health(self.docker, dep.container['Id'], dep.env.get('RELAY_LISTEN', '0.0.0.0:6689'), dep.db, old['version'], dep.role)
            old['version'] = current['version']
        target = args.version.lstrip('v') if args.version else latest_version(args.prerelease)
        key = version_key(target)
        if not key[3] and not args.prerelease:
            fail('预发布版本需要 --prerelease', 2)
        print(f'角色：{dep.role}；实例：{dep.project}/{dep.service}；目录：{dep.directory}\n当前版本：{old["version"]} → 目标版本：{target}')
        if target == old['version'] and not args.force:
            print('同版本，跳过升级。')
            return
        if version_key(target) < version_key(old['version']) and not args.allow_downgrade:
            fail('目标低于当前版本；需显式 --allow-downgrade，且 schema 必须兼容', 2)
        if not args.yes:
            if not sys.stdin.isatty():
                fail('非交互模式请使用 --yes', 2)
            if input('升级将短暂停止当前实例，输入 yes 继续：') != 'yes':
                print('已取消。')
                return
        image_ref = REPOSITORY + ':' + target
        self.docker.run(['docker', 'pull', image_ref], stage='拉取目标镜像')
        image = self.docker.json(['docker', 'image', 'inspect', image_ref])[0]
        arch = {'x86_64': 'amd64', 'aarch64': 'arm64'}.get(platform.machine())
        if image.get('Os') != 'linux' or image.get('Architecture') != arch:
            fail('目标镜像架构与本机不一致', 3)
        candidate = probe_image(self.docker, image['Id'])
        if candidate['version'] != target:
            fail('镜像内容版本与所选标签不一致', 3)
        public = args.public_url or dep.public_url()
        if public:
            u = urlsplit(public)
            if u.scheme != 'https' or not u.hostname or u.username or u.password or u.query or u.fragment or u.path not in ('', '/'):
                fail('--public-url/配置入口必须为 HTTPS origin', 2)
        self.ensure_exclusive_data()
        self.disk_check()
        backups = dep.directory / 'backups'
        backups.mkdir(mode=0o700, exist_ok=True)
        os.chmod(backups, 0o700)
        self.backup = backups / ('upgrade-' + target + '-' + str(time.time_ns()))
        self.backup.mkdir(mode=0o700)
        self.state = {'format': 1, 'backup': str(self.backup), 'files': [str(f) for f in dep.files],
                      'directory': str(dep.directory), 'project': dep.project, 'service': dep.service,
                      'role': dep.role, 'database': dep.db, 'mount': dep.mount, 'old_image': old_image,
                      'old_digests': self.docker.json(['docker', 'image', 'inspect', old_image])[0].get('RepoDigests', []),
                      'old_version': old['version'], 'new_image': image['Id'], 'new_version': target,
                      'was_running': running, 'public_url': public}
        self.state['configs'] = self.snapshot_configs()
        self.record('prepared')
        try:
            self.record('stopping')
            self.stopped = True  # Set before the command: a signal may arrive just after Docker stops it.
            dep.command('stop', '-t', '20', dep.service)
            self.ensure_exclusive_data()
            if dep.pg:
                nonpublic = dep.postgres('psql', '-At', '-c', "SELECT count(*) FROM pg_namespace WHERE nspname NOT IN ('public','information_schema') AND nspname NOT LIKE 'pg_%'", text=True).strip()
                if nonpublic != '0':
                    fail('数据库包含其他用户 schema，需人工备份恢复', 4)
                connections = dep.postgres('psql', '-At', '-c', "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid()", text=True).strip()
                if connections != '0':
                    fail('数据库仍有其他连接，拒绝备份/回滚共享数据库', 4)
                with (self.backup / 'database.dump').open('wb') as output:
                    dep.postgres('pg_dump', '-Fc', stdout=output, stage='PostgreSQL 一致性备份')
                self.state['dump_sha256'] = file_hash(self.backup / 'database.dump')
            self.state['data_state'] = self.save_data(self.backup / 'data.tar.gz')
            if dep.pg:
                self.state['data_state']['schema'] = int(dep.postgres('psql', '-At', '-c', 'SELECT MAX(version) FROM schema_migrations', text=True).strip())
            schema = self.state['data_state']['schema']
            if not isinstance(schema, int) or schema > candidate['schema'] or schema > old['schema']:
                fail('数据库 schema 高于目标程序，不能降级', 4)
            self.state['data_sha256'] = file_hash(self.backup / 'data.tar.gz')
            self.backup_ready = True
            self.record('backed-up')
            self.mutated = True
            self.record('upgrading')
            config = read_config(dep.files[-1])
            config['services'].setdefault(dep.service, {})['image'] = image_ref
            config['services'][dep.service]['pull_policy'] = 'never'
            private_write(dep.files[-1], encode_config(config))
            self.start_image(image['Id'])
            self.wait(target, image['Id'])
            self.verify_identity()
            if dep.pg:
                actual_schema = int(dep.postgres('psql', '-At', '-c', 'SELECT MAX(version) FROM schema_migrations', text=True).strip())
                if actual_schema != candidate['schema']:
                    fail('升级后的 PostgreSQL schema 不匹配', 5)
            if dep.db == 'sqlite':
                self.verify_sqlite(candidate['schema'])
            check_public(public, target, dep.role, dep.db)
            self.record('complete')
            print('升级成功；一致性备份保留：' + str(self.backup))
            if dep.role == 'agent':
                print('Agent 本机健康已确认；请在控制端确认心跳、配置版本同步和累计计量未回退。')
        except BaseException:
            # Do not let a second signal interrupt recovery, and never delete backups.
            with ignore_signals():
                try:
                    if self.mutated:
                        if not self.backup_ready:
                            fail('备份未完成，不能恢复', 6)
                        self.restore()
                    elif self.stopped:
                        if running:
                            dep.command('start', dep.service)
                            self.wait(old['version'], old_image)
                        self.record('cancelled')
                except BaseException as recovery_error:
                    if isinstance(recovery_error, UpgradeError):
                        print('恢复失败原因：' + str(recovery_error), file=sys.stderr)
                    with contextlib.suppress(BaseException):
                        dep.command('stop', '-t', '20', dep.service)
                    self.record('recovery-required')
                    print('恢复未通过验收，应用保持停止，备份未删除。恢复命令：\n' +
                          shlex.join(['python3', str(Path(__file__).resolve()), '--restore', str(self.backup), '--yes']), file=sys.stderr)
                    fail('自动恢复失败，请按保留备份人工恢复', 6)
            raise


@contextlib.contextmanager
def ignore_signals():
    saved = {}
    for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
        saved[sig] = signal.signal(sig, signal.SIG_IGN)
    try:
        yield
    finally:
        for sig, handler in saved.items():
            signal.signal(sig, handler)


def interrupted(signum, frame):
    raise UpgradeError('收到中断信号，已进入恢复处理', 128 + signum)


def parser():
    p = argparse.ArgumentParser(description='EmbyRelay 通用升级：自动识别控制端/Agent，备份、验收与失败回滚。')
    p.add_argument('--detect-only', action='store_true', help=argparse.SUPPRESS)
    p.add_argument('--directory', help='部署目录；默认从当前目录、安装入口和 Compose 容器发现')
    p.add_argument('--file', action='append', help='Compose 文件，可重复指定并保持原顺序')
    p.add_argument('--role', choices=['control', 'agent'])
    p.add_argument('--instance', help='明确选择 PROJECT/SERVICE')
    p.add_argument('--version', help='指定 X.Y.Z；默认 Docker Hub 最新正式版本，不依赖 Release')
    p.add_argument('--prerelease', action='store_true', help='允许显式或自动选择预发布')
    p.add_argument('--allow-downgrade', action='store_true')
    p.add_argument('--force', action='store_true', help='同版本也执行备份与重建')
    p.add_argument('--yes', '--non-interactive', action='store_true', help='无需确认；有歧义仍报错')
    p.add_argument('--public-url', help='HTTPS origin，供外部反代的验收')
    p.add_argument('--old-image', help='容器缺失时明确提供已记录的旧镜像 sha256 ID')
    p.add_argument('--restore', help='恢复本升级器的完整事务备份（包括原镜像）')
    p.add_argument('--health-timeout', type=int, default=90)
    return p


def main(argv=None):
    args = parser().parse_args(argv)
    if platform.system() != 'Linux' or os.geteuid() != 0:
        fail('仅支持 Linux root；请使用 sudo，需本机 Docker Engine 和 Compose v2.20+', 2)
    if not 1 <= args.health_timeout <= 600:
        fail('--health-timeout 需在 1–600 秒之间', 2)
    if args.version:
        version_key(args.version)
    if args.restore and (args.directory or args.file or args.instance or args.version):
        fail('--restore 不得与目标实例/版本选择混用，恢复目标由备份确定', 2)
    os.umask(0o077)
    docker = Docker()
    docker.run(['docker', 'info'], stage='连接 Docker Engine')
    compose_version = docker.run(['docker', 'compose', 'version', '--short'], text=True).strip().lstrip('v')
    if tuple(map(int, compose_version.split('.')[:2])) < (2, 20):
        fail('需要 Docker Compose v2.20 或更高版本', 2)
    context = docker.json(['docker', 'context', 'inspect'])[0]
    endpoint = os.environ.get('DOCKER_HOST') or context['Endpoints']['docker']['Host']
    if not endpoint.startswith('unix://'):
        fail('仅支持本机 Unix socket Docker Engine，禁止对远程 daemon 读写本地数据', 2)
    if args.restore:
        backup = Path(args.restore).resolve()
        if backup.stat().st_uid != 0 or backup.stat().st_mode & 0o077:
            fail('恢复目录必须为 root 所有且权限 0700', 2)
        state = json.loads((backup / 'manifest.json').read_text())
        if state.get('format') != 1 or not state.get('data_sha256') and state.get('mutated', True):
            fail('事务备份尚未完整且可能已修改数据，不能恢复', 4)
        dep = Deployment(docker, state['files'], state['project'], state['service'])
        if (dep.directory != Path(state['directory']) or dep.role != state['role'] or dep.db != state['database'] or dep.mount != state.get('mount')):
            fail('备份与当前部署不匹配', 2)
    else:
        dep = discover(docker, args, allow_missing=args.detect_only)
        if args.detect_only:
            if dep is None:
                fail('未发现已有部署', 8)
            print('发现已有部署，将进入通用升级流程。')
            return
    # Also coordinate with both original installers' directory locks.
    lock_path = dep.directory / '.install.lock'
    with lock_path.open('a') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            fail('此实例已有安装/升级任务运行', 7)
        tx = Transaction(dep, args.health_timeout)
        for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
            signal.signal(sig, interrupted)
        if args.restore:
            if not args.yes and (not sys.stdin.isatty() or input('恢复将覆盖当前数据，输入 RESTORE 继续：') != 'RESTORE'):
                fail('恢复需交互确认或 --yes', 2)
            tx.backup, tx.state = backup, state
            with ignore_signals():
                tx.restore()
        else:
            tx.execute(args)


if __name__ == '__main__':
    try:
        main()
    except UpgradeError as error:
        if error.code != 8:  # Internal presence check: absence is not an upgrade failure.
            print('升级失败：' + str(error), file=sys.stderr)
        sys.exit(error.code)
    except (OSError, ValueError, KeyError, TypeError, sqlite3.DatabaseError):
        print('升级失败：配置、备份或系统状态无效；敏感内容未输出。请检查权限及保留的事务记录。', file=sys.stderr)
        sys.exit(1)
