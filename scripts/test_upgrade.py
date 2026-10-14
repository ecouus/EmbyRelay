"""Public upgrader regression tests; no real deployment is touched."""
import argparse
import contextlib
import io
import importlib.util
import json
from pathlib import Path
import signal
import sqlite3
import tarfile
import tempfile
import unittest
from unittest.mock import Mock, patch

spec = importlib.util.spec_from_file_location('upgrade', Path(__file__).with_name('upgrade.py'))
u = importlib.util.module_from_spec(spec)
spec.loader.exec_module(u)
OLD = 'sha256:' + '1' * 64
NEW = 'sha256:' + '2' * 64


class UpgradeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.data = self.root / 'data'
        self.data.mkdir()
        self.path = self.root / 'docker-compose.yml'
        self.config = {'name': 'custom-project', 'services': {'custom-service': {
            'image': 'ecouus/embyrelay:1.0.18', 'environment': {'RELAY_LISTEN': '0.0.0.0:9123', 'RELAY_DATA_DIR': '/data'},
            'volumes': [{'type': 'bind', 'source': str(self.data), 'target': '/data'}]}}}
        self.path.write_text(json.dumps(self.config))
        self.docker = Mock(spec=u.Docker)
        self.docker.json.side_effect = lambda cmd, **kw: self.config if 'config' in cmd else [{'Id': OLD if cmd[-1] == OLD else NEW, 'Os': 'linux', 'Architecture': 'amd64', 'RepoDigests': []}]
        self.docker.containers.return_value = []
        self.container = {'Id': 'container', 'Image': OLD, 'State': {'Running': True},
                          'Mounts': [{'Destination': '/data', 'Source': str(self.data), 'Type': 'bind'}],
                          'Config': {'Image': 'ecouus/embyrelay:1.0.18', 'Entrypoint': ['/usr/local/bin/embyrelay'],
                                     'Env': ['RELAY_DATA_DIR=/data'], 'Labels': {'com.docker.compose.project': 'custom-project',
                                      'com.docker.compose.service': 'custom-service'}}}
        self.dep = u.Deployment(self.docker, [self.path], container=self.container)
        self.args = u.parser().parse_args(['--version', '1.0.21', '--yes'])
        self.db = self.data / 'embyrelay.db'
        with contextlib.closing(sqlite3.connect(self.db)) as conn:
            conn.executescript('CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY);INSERT INTO schema_migrations VALUES(14);CREATE TABLE settings(key TEXT PRIMARY KEY,value TEXT);')
        (self.data / 'master.key').write_text('private-test-key')

    def probe(self, docker, image):
        return {'version': '1.0.18' if image == OLD else '1.0.21', 'schema': 14 if image == OLD else 15}

    def run_transaction(self, fail_wait=False, backup_error=False, restore_error=False):
        tx = u.Transaction(self.dep)
        def save(destination):
            if backup_error:
                raise u.UpgradeError('backup injected', 4)
            with tarfile.open(destination, 'w:gz') as archive:
                for file in self.data.iterdir():
                    archive.add(file, arcname=file.name)
            return u.archive_state(destination, 'control', 'sqlite')
        with patch.object(u, 'probe_image', side_effect=self.probe), patch.object(u, 'health', return_value={'version': '1.0.18'}), \
             patch.object(u.platform, 'machine', return_value='x86_64'), patch.object(tx, 'disk_check'), \
             patch.object(tx, 'save_data', side_effect=save), patch.object(tx, 'verify_identity'), \
             patch.object(tx, 'verify_sqlite'), patch.object(tx, 'start_image'), patch.object(tx, 'wait') as wait, \
             patch.object(u, 'check_public'), contextlib.redirect_stdout(io.StringIO()):
            wait.side_effect = [u.UpgradeError('health injected', 5), None] if fail_wait else None
            if restore_error:
                with patch.object(tx, 'restore', side_effect=u.UpgradeError('restore injected', 6)):
                    with self.assertRaises(u.UpgradeError) as error:
                        tx.execute(self.args)
                    self.assertEqual(error.exception.code, 6)
            elif fail_wait or backup_error:
                with self.assertRaises(u.UpgradeError):
                    tx.execute(self.args)
            else:
                tx.execute(self.args)
        return tx

    def test_roles_and_custom_names(self):
        self.assertEqual(self.dep.role, 'control')
        self.assertEqual(self.dep.service, 'custom-service')
        self.assertEqual(u.role_of({'image': 'ecouus/embyrelay:1.0.21', 'entrypoint': ['/usr/local/bin/embyrelay-agent']}), 'agent')
        self.assertIsNone(u.role_of({'image': 'unrelated/service:latest'}))

    def test_custom_entrypoint_is_rejected(self):
        with self.assertRaises(u.UpgradeError):
            u.role_of({'image': 'ecouus/embyrelay:latest', 'entrypoint': ['sh', '-c', 'arbitrary']})

    def test_mount_drift_is_rejected(self):
        self.container['Mounts'][0]['Source'] = '/different'
        with self.assertRaisesRegex(u.UpgradeError, '挂载'):
            u.Deployment(self.docker, [self.path], container=self.container)

    def test_database_drift_is_rejected(self):
        self.container['Config']['Env'].append('RELAY_DB_DRIVER=postgres')
        with self.assertRaisesRegex(u.UpgradeError, '漂移'):
            u.Deployment(self.docker, [self.path], container=self.container)

    def test_mixed_roles_need_explicit_service(self):
        self.config['services']['second'] = dict(self.config['services']['custom-service'], entrypoint=['/usr/local/bin/embyrelay-agent'])
        with self.assertRaisesRegex(u.UpgradeError, '多个'):
            u.Deployment(self.docker, [self.path])
        dep = u.Deployment(self.docker, [self.path], service='second')
        self.assertEqual(dep.role, 'agent')

    def test_safe_yaml_anchors_and_duplicate_rejection(self):
        self.path.write_text('services:\n  app:\n    image: ecouus/embyrelay:1.0.18\n    environment: &env\n      SECRET: "$literal"\n  second:\n    environment: *env\n')
        self.assertEqual(u.read_config(self.path)['services']['second']['environment']['SECRET'], '$literal')
        for text in ['services: {}\nservices: {}', 'services: !!python/object/apply:os.system ["touch /tmp/forbidden"]']:
            self.path.write_text(text)
            with self.assertRaises(u.UpgradeError):
                u.read_config(self.path)

    def test_include_and_json_duplicates_rejected(self):
        for text in ['{"services":{},"services":{}}', '{"services":{},"include":["https://example.com/compose.yml"]}']:
            self.path.write_text(text)
            with self.assertRaises(u.UpgradeError):
                u.read_config(self.path)

    def test_semver_and_prerelease_ordering(self):
        order = ['1.0.0-alpha.2', '1.0.0-alpha.10', '1.0.0-beta', '1.0.0', '1.0.10', '1.1.0']
        self.assertEqual(sorted(reversed(order), key=u.version_key), order)
        for value in ['latest', '1.0', '1.0.01', '1.0.0-01', '1.0.0;exit']:
            with self.assertRaises(u.UpgradeError):
                u.version_key(value)

    def test_latest_does_not_depend_on_github_release(self):
        with patch.object(u, 'request_json', return_value={'results': [{'name': n} for n in ['latest', '1.0.20', '1.0.21', '1.1.0-rc.1']], 'next': None}):
            self.assertEqual(u.latest_version(), '1.0.21')
            self.assertEqual(u.latest_version(True), '1.1.0-rc.1')

    def test_network_and_auth_failure_are_errors(self):
        with patch.object(u.urllib.request, 'urlopen', side_effect=OSError('password-never-print')):
            with self.assertRaises(u.UpgradeError) as error:
                u.latest_version()
        self.assertEqual(error.exception.code, 3)
        self.assertNotIn('password-never-print', str(error.exception))

    def test_untrusted_pagination_rejected(self):
        with patch.object(u, 'request_json', return_value={'results': [], 'next': 'http://untrusted/'}):
            with self.assertRaises(u.UpgradeError):
                u.latest_version()

    def test_same_version_no_pull_or_stop(self):
        self.args.version = '1.0.18'
        with patch.object(u, 'probe_image', side_effect=self.probe), patch.object(u, 'health', return_value={'version': '1.0.18'}):
            u.Transaction(self.dep).execute(self.args)
        self.docker.run.assert_not_called()

    def test_prerelease_requires_explicit_selection(self):
        self.args.version = '1.1.0-rc.1'
        with patch.object(u, 'probe_image', side_effect=self.probe), patch.object(u, 'health', return_value={'version': '1.0.18'}):
            with self.assertRaisesRegex(u.UpgradeError, '预发布'):
                u.Transaction(self.dep).execute(self.args)

    def test_downgrade_requires_explicit_flag(self):
        self.args.version = '1.0.17'
        with patch.object(u, 'probe_image', side_effect=self.probe), patch.object(u, 'health', return_value={'version': '1.0.18'}):
            with self.assertRaisesRegex(u.UpgradeError, '低于'):
                u.Transaction(self.dep).execute(self.args)

    def test_fixed_version_updated_and_full_backup_private(self):
        self.config['services']['custom-service']['environment']['LITERAL'] = '$$unchanged'
        self.path.write_text(json.dumps(self.config))
        tx = self.run_transaction()
        saved = json.loads(self.path.read_text())
        self.assertEqual(saved['services']['custom-service']['image'], 'ecouus/embyrelay:1.0.21')
        self.assertEqual(saved['services']['custom-service']['environment']['LITERAL'], '$$unchanged')
        self.assertEqual(tx.state['phase'], 'complete')
        self.assertEqual(tx.backup.stat().st_mode & 0o777, 0o700)
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o600)
        self.assertEqual(tx.state['old_image'], OLD)
        self.assertIn('master.key', tx.state['data_state']['hashes'])

    def test_health_failure_restores_original_config_and_data(self):
        original = self.path.read_bytes()
        tx = self.run_transaction(fail_wait=True)
        self.assertEqual(self.path.read_bytes(), original)
        self.assertEqual(tx.state['phase'], 'rolled-back')
        self.assertTrue((tx.backup / 'data.tar.gz').exists())
        calls = str(self.dep.docker.run.call_args_list)
        self.assertNotIn('down', calls)

    def test_backup_failure_restarts_old_without_mutation(self):
        original = self.path.read_bytes()
        tx = self.run_transaction(backup_error=True)
        self.assertEqual(self.path.read_bytes(), original)
        self.assertEqual(tx.state['phase'], 'cancelled')
        self.assertFalse(tx.mutated)

    def test_rollback_failure_keeps_backup_and_stops_service(self):
        tx = self.run_transaction(fail_wait=True, restore_error=True)
        self.assertEqual(tx.state['phase'], 'recovery-required')
        self.assertTrue((tx.backup / 'data.tar.gz').exists())

    def test_agent_identity_required_and_hashed(self):
        archive = self.root / 'agent.tar.gz'
        with tarfile.open(archive, 'w:gz') as saved:
            saved.add(self.data / 'master.key', arcname='master.key')
        with self.assertRaisesRegex(u.UpgradeError, '身份'):
            u.archive_state(archive, 'agent', 'postgres')
        (self.data / 'identity.json').write_text('{"id":"unchanged"}')
        with tarfile.open(archive, 'w:gz') as saved:
            saved.add(self.data / 'master.key', arcname='master.key')
            saved.add(self.data / 'identity.json', arcname='identity.json')
        self.assertIn('identity.json', u.archive_state(archive, 'agent', 'postgres')['hashes'])

    def test_archive_traversal_rejected(self):
        archive = self.root / 'bad.tar.gz'
        with tarfile.open(archive, 'w:gz') as saved:
            member = tarfile.TarInfo('../escape')
            saved.addfile(member)
        with self.assertRaises(u.UpgradeError):
            u.validate_archive(archive)

    def test_shared_writer_rejected(self):
        self.docker.containers.return_value = [{'Id': 'other', 'State': {'Running': True}, 'Mounts': [{'Source': str(self.data), 'RW': True}]}]
        with self.assertRaisesRegex(u.UpgradeError, '其他'):
            u.Transaction(self.dep).ensure_exclusive_data()

    def test_disk_space_prevents_stop(self):
        with patch.object(u.shutil, 'disk_usage', return_value=Mock(free=1)):
            with self.assertRaisesRegex(u.UpgradeError, '空间'):
                u.Transaction(self.dep).disk_check()

    def test_signal_reports_recoverable_interrupt(self):
        with self.assertRaises(u.UpgradeError) as error:
            u.interrupted(signal.SIGTERM, None)
        self.assertEqual(error.exception.code, 143)

    def test_runtime_override_is_not_a_new_base_file(self):
        backup = self.root / 'backup'
        backup.mkdir()
        runtime = backup / 'runtime-override.json'
        runtime.write_text('{}')
        u.save_json(backup / 'manifest.json', {'format': 1, 'project': 'p', 'service': 's', 'files': [str(self.path)]})
        self.assertEqual(u.original_files([str(self.path), str(runtime)], 'p', 's'), [str(self.path)])

    def test_external_postgres_rejected(self):
        self.config['services']['custom-service']['environment'].update(RELAY_DB_DRIVER='postgres', RELAY_DATABASE_URL='postgres://user:private@outside/db')
        with self.assertRaisesRegex(u.UpgradeError, '专用 PostgreSQL'):
            u.Deployment(self.docker, [self.path], service='custom-service')

    def test_unknown_existing_container_requires_old_id(self):
        self.dep.container = None
        with self.assertRaisesRegex(u.UpgradeError, 'old-image'):
            u.Transaction(self.dep).execute(self.args)

    def test_pull_auth_failure_does_not_stop_application(self):
        self.docker.run.side_effect = u.UpgradeError('pull authentication failed', 3)
        with patch.object(u, 'probe_image', side_effect=self.probe), patch.object(u, 'health', return_value={'version': '1.0.18'}):
            tx = u.Transaction(self.dep)
            with self.assertRaises(u.UpgradeError) as error:
                tx.execute(self.args)
            self.assertEqual(error.exception.code, 3)
            self.assertFalse(tx.stopped)
            self.assertFalse(tx.state_path.exists())

    def test_image_architecture_mismatch_rejected_before_stop(self):
        self.docker.json.side_effect = lambda cmd, **kw: [{'Id': NEW, 'Os': 'linux', 'Architecture': 'arm64'}]
        with patch.object(u, 'probe_image', side_effect=self.probe), patch.object(u, 'health', return_value={'version': '1.0.18'}), patch.object(u.platform, 'machine', return_value='x86_64'):
            tx = u.Transaction(self.dep)
            with self.assertRaisesRegex(u.UpgradeError, '架构'):
                tx.execute(self.args)
            self.assertFalse(tx.stopped)

    def test_image_content_version_mismatch_rejected_before_stop(self):
        def probe(docker, image):
            return {'version': '1.0.18' if image == OLD else '1.0.22', 'schema': 15}
        with patch.object(u, 'probe_image', side_effect=probe), patch.object(u, 'health', return_value={'version': '1.0.18'}), patch.object(u.platform, 'machine', return_value='x86_64'):
            tx = u.Transaction(self.dep)
            with self.assertRaisesRegex(u.UpgradeError, '内容版本'):
                tx.execute(self.args)
            self.assertFalse(tx.stopped)

    def test_https_checks_version_and_rejects_redirects(self):
        response = Mock()
        response.__enter__ = Mock(return_value=io.StringIO('{"status":"ok","version":"1.0.18"}'))
        response.__exit__ = Mock(return_value=False)
        opener = Mock();opener.open.return_value = response
        with patch.object(u.urllib.request, 'build_opener', return_value=opener):
            with self.assertRaisesRegex(u.UpgradeError, 'HTTPS'):
                u.check_public('https://relay.example.com', '1.0.21', 'control', 'sqlite')
        with self.assertRaises(u.UpgradeError):
            u.check_public('https://private:secret@relay.example.com', '1.0.21', 'control', 'sqlite')

    def test_partial_backup_recovers_without_replacing_data(self):
        tx = u.Transaction(self.dep)
        tx.state = {'mutated': False, 'old_image': OLD, 'old_version': '1.0.18', 'was_running': True,
                    'configs': [{'path': str(self.path), 'sha256': u.file_hash(self.path)}]}
        self.docker.run.return_value = 'container'
        self.docker.inspect.return_value = self.container
        original = self.db.read_bytes()
        with patch.object(tx, 'wait'):
            tx.restore()
        self.assertEqual(self.db.read_bytes(), original)
        self.assertEqual(tx.state['phase'], 'cancelled')
        self.assertFalse(tx.state['mutated'])

    def test_partial_backup_after_possible_mutation_is_rejected(self):
        tx = u.Transaction(self.dep);tx.state = {'mutated': True}
        with self.assertRaisesRegex(u.UpgradeError, '缺少完整备份'):
            tx.restore()

    def test_restore_helper_accepts_backup_stdin(self):
        self.dep.helper(OLD, 'tar xzf - -C /snapshot', writable=True, stdin=io.BytesIO(b'test'))
        command = self.docker.run.call_args.args[0]
        self.assertIn('-i', command)
        self.assertNotIn(',readonly', ' '.join(command))

    def test_private_write_does_not_execute_interpolation(self):
        u.private_write(self.path, '{"services":{"s":{"image":"$(touch /tmp/not-run)"}}}')
        self.assertIn('$(touch', u.read_config(self.path)['services']['s']['image'])


if __name__ == '__main__':
    unittest.main()
