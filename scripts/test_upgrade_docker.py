"""Opt-in disposable Docker upgrades. No image build/push or production access.
EMBYRELAY_DOCKER_TEST=1 python3 -m unittest scripts.test_upgrade_docker
"""
import contextlib
import hashlib
import http.cookiejar
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import unittest
import urllib.request
import uuid
from unittest.mock import patch
from scripts.test_upgrade import u


@unittest.skipUnless(os.getenv('EMBYRELAY_DOCKER_TEST') == '1', 'opt-in disposable Docker integration')
class DockerUpgradeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='embyrelay-upgrade-test-')
        self.root = Path(self.temp.name)
        self.project = 'embyrelay-upgrade-test-' + uuid.uuid4().hex[:12]
        self.path = self.root / 'docker-compose.yml'
        self.docker = u.Docker()
        self.compose = ['docker', 'compose', '-p', self.project, '-f', str(self.path)]
        self.addCleanup(self.cleanup)
        self.data = self.root / 'data'
        self.data.mkdir();os.chown(self.data, 10001, 10001)
        self.config = {'name': self.project, 'services': {'panel': {
            'image': 'ecouus/embyrelay:1.0.18', 'environment': {'RELAY_DATA_DIR': '/data', 'RELAY_LISTEN': '0.0.0.0:6689',
            'RELAY_ADMIN_USER': 'upgrade-test-admin', 'RELAY_ADMIN_PASSWORD': 'isolated-test-password', 'RELAY_ALLOW_INSECURE': 'true'},
            'volumes': [str(self.data) + ':/data'], 'read_only': True}}}

    def cleanup(self):
        if self.path.exists():
            subprocess.run(self.compose + ['down', '--volumes', '--remove-orphans'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.temp.cleanup()

    def save(self):
        u.save_json(self.path, self.config)

    def container(self, service='panel'):
        return self.docker.run(self.compose + ['ps', '-aq', service], text=True).strip()

    def wait_old(self, service='panel', role='control', database='sqlite'):
        for _ in range(60):
            try:
                return u.health(self.docker, self.container(service), '0.0.0.0:6689', database, '1.0.18', role)
            except u.UpgradeError:
                time.sleep(.3)
        self.fail('old test service did not start')

    def install(self, database='sqlite'):
        if database == 'postgres':
            svc = self.config['services']['panel']
            svc['environment'].update(RELAY_DB_DRIVER='postgres', RELAY_DATABASE_URL='postgres://upgrade_test:isolated-db-password@database:5432/upgrade_test?sslmode=disable')
            svc['depends_on'] = {'database': {'condition': 'service_healthy'}}
            self.config['services']['database'] = {'image': 'postgres:17-alpine',
                'environment': {'POSTGRES_USER': 'upgrade_test', 'POSTGRES_DB': 'upgrade_test', 'POSTGRES_PASSWORD': 'isolated-db-password'},
                'volumes': ['pgdata:/var/lib/postgresql/data'], 'healthcheck': {'test': ['CMD-SHELL', 'pg_isready -U upgrade_test'], 'interval': '1s', 'timeout': '2s', 'retries': 30}}
            self.config['volumes'] = {'pgdata': {}}
        self.save();self.docker.run(self.compose + ['up', '-d'], stage='启动隔离测试')
        self.wait_old(database=database)

    def dep(self, service='panel'):
        args = u.parser().parse_args(['--file', str(self.path), '--instance', self.project + '/' + service, '--yes'])
        return u.discover(self.docker, args)

    def upgrade(self, service='panel', failure=False, stopped=False):
        dep = self.dep(service)
        tx = u.Transaction(dep, timeout=15)
        args = u.parser().parse_args(['--version', '1.0.21', '--yes'])
        original = self.path.read_bytes()
        before = dep.container['Image']
        key = hashlib.sha256((self.data / 'master.key').read_bytes()).hexdigest()
        identity = (self.data / 'identity.json').read_bytes() if service == 'node' else None
        if failure:
            def acceptance(url, expected, role, database):
                if expected == '1.0.21':
                    # Simulate a failure only after real new-version migrations and startup.
                    if database == 'postgres':
                        dep.postgres('psql', '-c', "CREATE TABLE rollback_only_new(value text);INSERT INTO settings VALUES('rollback_marker','new')")
                    raise u.UpgradeError('injected HTTPS failure after migrations', 5)
            with patch.object(u, 'check_public', side_effect=acceptance):
                with self.assertRaises(u.UpgradeError):
                    tx.execute(args)
            self.assertEqual(self.path.read_bytes(), original)
            self.assertEqual(self.docker.inspect(self.container(service))['Image'], before)
            self.assertEqual(tx.state['phase'], 'rolled-back')
            self.assertEqual(self.docker.inspect(self.container(service))['State']['Running'], not stopped)
        else:
            tx.execute(args)
            self.assertEqual(tx.state['phase'], 'complete')
            self.assertNotEqual(self.docker.inspect(self.container(service))['Image'], before)
            self.assertEqual(json.loads(self.path.read_text())['services'][service]['image'], 'ecouus/embyrelay:1.0.21')
            again = self.dep(service)
            self.assertEqual(again.files, [self.path])
            # Repeated same-version invocation must keep the current container and backup.
            cid = self.container(service)
            u.Transaction(again).execute(args)
            self.assertEqual(self.container(service), cid)
        self.assertEqual(hashlib.sha256((self.data / 'master.key').read_bytes()).hexdigest(), key)
        if identity:
            self.assertEqual((self.data / 'identity.json').read_bytes(), identity)
        return tx

    def sql(self, query):
        return self.docker.run(self.compose + ['exec', '-T', 'database', 'psql', '-U', 'upgrade_test', '-d', 'upgrade_test', '-At', '-c', query], text=True).strip()

    def test_control_sqlite_upgrade(self):
        self.install();self.upgrade()
        result = subprocess.run(['bash', str(Path(__file__).with_name('upgrade.sh')), '--directory', str(self.root), '--instance', self.project + '/panel', '--version', '1.0.21', '--yes'], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('同版本', result.stdout)
        result = subprocess.run(['bash', str(Path(__file__).with_name('install.sh')), 'upgrade', '--directory', str(self.root), '--version', '1.0.21', '--yes'], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        result = subprocess.run(['bash', str(Path(__file__).with_name('install.sh')), '--directory', str(self.root), '--version', '1.0.21', '--yes'], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('同版本', result.stdout)

    def test_control_sqlite_rollback(self):
        self.install();self.upgrade(failure=True)

    def test_postgres_upgrade(self):
        self.install('postgres');self.upgrade()
        self.assertEqual(self.sql('SELECT MAX(version) FROM schema_migrations'), '15')

    def test_postgres_rollback_removes_new_objects(self):
        self.install('postgres')
        old_schema = self.sql('SELECT MAX(version) FROM schema_migrations')
        self.upgrade(failure=True)
        self.assertEqual(self.sql('SELECT MAX(version) FROM schema_migrations'), old_schema)
        self.assertEqual(self.sql("SELECT count(*) FROM settings WHERE key='rollback_marker'"), '0')
        self.assertEqual(self.sql("SELECT count(*) FROM information_schema.tables WHERE table_name='rollback_only_new'"), '0')

    def install_agent(self):
        # Real enrollment against a separate disposable controller, no invented identity.
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0));port = sock.getsockname()[1]
        self.config['services']['panel']['ports'] = [f'127.0.0.1:{port}:6689']
        control_data = self.root / 'control-data';control_data.mkdir();os.chown(control_data, 10001, 10001)
        self.config['services']['panel']['volumes'] = [str(control_data) + ':/data']
        self.save();self.docker.run(self.compose + ['up', '-d']);self.wait_old()
        opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
        base = 'http://127.0.0.1:' + str(port)
        def api(path, body):
            req = urllib.request.Request(base + '/api/v1/' + path, data=json.dumps(body).encode(), headers={'Content-Type': 'application/json', 'X-Requested-With': 'EmbyRelay', 'Origin': base})
            with opener.open(req) as response:return json.load(response)
        api('auth/login', {'username': 'upgrade-test-admin', 'password': 'isolated-test-password'})
        enroll = api('relays/enroll', {})
        self.config['services']['node'] = {'image': 'ecouus/embyrelay:1.0.18', 'entrypoint': ['/usr/local/bin/embyrelay-agent'],
            'read_only': True, 'volumes': [str(self.data) + ':/data'],
            'environment': {'RELAY_DATA_DIR': '/data', 'RELAY_LISTEN': '0.0.0.0:6689', 'RELAY_ALLOW_INSECURE': 'true',
                            'RELAY_CONTROL_URL': 'http://panel:6689', 'RELAY_PUBLIC_URL': 'http://node:6689',
                            'RELAY_ENROLL_TOKEN': enroll['token'], 'RELAY_SERVER_KEY': enroll['server_key'], 'RELAY_NODE_NAME': 'isolated-test-node'}}
        self.save();self.docker.run(self.compose + ['up', '-d', 'node']);self.wait_old('node', 'agent')
        # Registration token is no longer needed; simulate both installers' cleanup.
        self.config['services']['node']['environment']['RELAY_ENROLL_TOKEN'] = ''
        self.config['services']['node']['environment'].pop('RELAY_PUBLIC_URL')
        self.save();self.docker.run(self.compose + ['up', '-d', '--no-deps', 'node']);self.wait_old('node', 'agent')

    def test_agent_upgrade_preserves_registered_identity(self):
        self.install_agent();self.upgrade('node')
        result = subprocess.run(['bash', str(Path(__file__).with_name('install-agent.sh')), 'upgrade', '--directory', str(self.root), '--version', '1.0.21', '--yes'], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        for action in ([], ['upgrade']):
            result = subprocess.run(['bash', str(Path(__file__).with_name('install.sh'))] + action + ['--directory', str(self.root), '--instance', self.project + '/node', '--version', '1.0.21', '--yes'], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('同版本', result.stdout)

    def test_agent_rollback_preserves_registered_identity(self):
        self.install_agent();self.upgrade('node', failure=True)

    def test_stopped_control_rolls_back_to_stopped(self):
        self.install();self.docker.run(self.compose + ['stop', 'panel']);self.upgrade(failure=True, stopped=True)

    def test_missing_container_requires_captured_old_image(self):
        self.install();old = self.docker.inspect(self.container())['Image']
        self.docker.run(self.compose + ['stop', 'panel']);self.docker.run(self.compose + ['rm', '-f', 'panel'])
        dep = self.dep();self.assertIsNone(dep.container)
        args = u.parser().parse_args(['--version', '1.0.21', '--yes', '--old-image', old])
        tx = u.Transaction(dep, 15);tx.execute(args);self.assertEqual(tx.state['phase'], 'complete')

    def test_downgrade_newer_schema_is_rejected_without_data_change(self):
        self.install();self.upgrade()
        dep = self.dep();before = dep.container['Image']
        tx = u.Transaction(dep, 15)
        args = u.parser().parse_args(['--version', '1.0.18', '--allow-downgrade', '--yes'])
        with self.assertRaisesRegex(u.UpgradeError, 'schema'):
            tx.execute(args)
        self.assertEqual(tx.state['phase'], 'cancelled')
        self.assertEqual(self.docker.inspect(self.container())['Image'], before)
        u.health(self.docker, self.container(), '0.0.0.0:6689', 'sqlite', '1.0.21', 'control')

    def test_manual_restore_after_success_returns_matching_old_schema(self):
        self.install();tx = self.upgrade()
        result = subprocess.run(['bash', str(Path(__file__).with_name('upgrade.sh')), '--restore', str(tx.backup), '--yes'], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        u.health(self.docker, self.container(), '0.0.0.0:6689', 'sqlite', '1.0.18', 'control')

    def test_interrupted_before_backup_restores_old_running_state(self):
        self.install();dep = self.dep();tx = u.Transaction(dep, 15)
        tx.backup = self.root / 'partial-backup';tx.backup.mkdir(mode=0o700)
        tx.state = {'format': 1, 'backup': str(tx.backup), 'files': [str(self.path)],
                    'directory': str(self.root), 'project': self.project, 'service': 'panel',
                    'role': 'control', 'database': 'sqlite', 'mount': dep.mount,
                    'old_image': dep.container['Image'], 'old_version': '1.0.18', 'was_running': True}
        tx.state['configs'] = tx.snapshot_configs();tx.record('stopping')
        dep.command('stop', '-t', '20', 'panel')
        result = subprocess.run(['bash', str(Path(__file__).with_name('upgrade.sh')), '--restore', str(tx.backup), '--yes'], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.wait_old()
        self.assertEqual(json.loads((tx.backup / 'manifest.json').read_text())['phase'], 'cancelled')

    def test_instance_lock_rejects_concurrent_upgrade(self):
        self.install();before = self.container()
        with (self.root / '.install.lock').open('a') as lock:
            u.fcntl.flock(lock, u.fcntl.LOCK_EX | u.fcntl.LOCK_NB)
            result = subprocess.run(['bash', str(Path(__file__).with_name('upgrade.sh')), '--directory', str(self.root), '--version', '1.0.21', '--yes'], capture_output=True, text=True)
        self.assertEqual(result.returncode, 7, result.stderr)
        self.assertEqual(self.container(), before)
        self.wait_old()

    def test_ambiguous_instances_noninteractive_and_role_filter(self):
        self.install_agent()
        args = u.parser().parse_args(['--directory', str(self.root), '--yes'])
        with self.assertRaisesRegex(u.UpgradeError, '多个部署'):
            u.discover(self.docker, args)
        result = subprocess.run(['bash', str(Path(__file__).with_name('install.sh')), '--directory', str(self.root), '--yes'], capture_output=True, text=True)
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertIn('多个部署', result.stderr)
        self.assertNotIn('首次配置', result.stdout)
        args.role = 'agent'
        self.assertEqual(u.discover(self.docker, args).service, 'node')


if __name__ == '__main__':
    unittest.main()
