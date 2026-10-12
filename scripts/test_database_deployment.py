"""Opt-in real Docker lifecycle tests; all containers and volumes are temporary.

EMBYRELAY_DOCKER_TEST=1 EMBYRELAY_TEST_IMAGE=embyrelay:postgres-test \
    python3 -m unittest scripts/test_database_deployment.py
"""
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import types
import unittest
import uuid
from unittest.mock import patch

source = Path(__file__).with_name('install.sh').read_text().split("<<'PY'\n", 1)[1].rsplit('\nPY', 1)[0]
installer = types.ModuleType('docker_installer')
exec(compile(source, 'install.sh', 'exec'), installer.__dict__)
original_save_secrets = installer.save_database_secrets


@unittest.skipUnless(os.getenv('EMBYRELAY_DOCKER_TEST') == '1', 'opt-in Docker integration test')
class DockerDatabaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='embyrelay-db-test-', dir='/private/tmp' if Path('/private/tmp').exists() else '/tmp')
        self.directory = Path(self.temp.name)
        self.path = self.directory / 'docker-compose.yml'
        installer.PROJECT = 'embyrelay-db-test-' + uuid.uuid4().hex[:12]
        installer.IMAGE = os.getenv('EMBYRELAY_TEST_IMAGE', 'embyrelay:postgres-test')
        self.compose = ['docker', 'compose', '--project-name', installer.PROJECT,
                        '--project-directory', str(self.directory), '-f', str(self.path)]
        self.addCleanup(self.cleanup)
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            self.port = sock.getsockname()[1]

    def cleanup(self):
        subprocess.run(self.compose + ['down', '--volumes', '--remove-orphans'], check=False,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        pending = self.directory / 'docker-compose.migrate.yml'
        if pending.exists():
            subprocess.run(self.compose[:-1] + [str(pending), 'down', '--volumes', '--remove-orphans'],
                           check=False, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.temp.cleanup()

    def secrets(self, directory, secret, port):
        # Desktop test uses a bridge network. Production host-network config
        # continues to use 127.0.0.1. Set ownership through a Linux container.
        with patch.object(installer.os, 'chown'):
            original_save_secrets(directory, secret, port)
        file = directory / 'secrets/database_url'
        file.chmod(0o600)
        file.write_text(f'postgres://embyrelay:{secret}@postgres:5432/embyrelay?sslmode=disable\n')
        installer.run(['docker', 'run', '--rm', '-v', str(directory) + ':/test',
                       '--user', '0', '--entrypoint', 'sh', installer.IMAGE, '-c',
                       'chown 10001:10001 /test/secrets/database_url; chmod 400 /test/secrets/database_url'])

    def install(self, driver):
        config = installer.configuration('relay.example.com', 'admin', 'test-password-123',
                                         driver, 'x' * 48 if driver == 'postgres' else None, self.port)
        app = config['services']['embyrelay']
        # Exercise the same generated app with portable Docker Desktop networking.
        app.pop('network_mode')
        app['environment']['RELAY_LISTEN'] = '0.0.0.0:6689'
        if driver == 'postgres':
            self.secrets(self.directory, 'x' * 48, self.port)
        installer.save(self.path, config)
        installer.start(self.compose, self.path, config)
        return config

    def sql(self, query):
        return installer.run(self.compose + ['exec', '-T', 'postgres', 'psql', '-U', 'embyrelay',
                                             '-d', 'embyrelay', '-At', '-c', query],
                             capture_output=True, text=True).stdout.strip()

    def test_postgres_backup_restore_and_password_reset(self):
        config = self.install('postgres')
        self.assertEqual(self.sql('SELECT MAX(version) FROM schema_migrations'), '13')
        self.sql("INSERT INTO settings VALUES('integration_marker','before')")
        installer.backup(self.compose, self.directory, config)
        backup = next((self.directory / 'backups').glob('*.tar.gz'))
        self.sql("UPDATE settings SET value='after' WHERE key='integration_marker'")
        with patch.object(installer, 'ask', return_value='RESTORE'):
            installer.restore(self.compose, self.directory, config, backup)
        self.assertEqual(self.sql("SELECT value FROM settings WHERE key='integration_marker'"), 'before')
        installer.reset_password(self.compose, 'admin', 'replacement-password-123')
        installer.healthy(self.compose, 'postgres')

    def test_sqlite_migration_resumes_after_committed_copy(self):
        config = self.install('sqlite')
        original_run = installer.run
        interrupted = False
        def run(command, **kwargs):
            nonlocal interrupted
            result = original_run(command, **kwargs)
            if '--migration-id' in command and not interrupted:
                interrupted = True
                raise RuntimeError('simulated interruption after commit')
            return result
        with patch.object(installer, 'save_database_secrets', side_effect=self.secrets), \
             patch.object(installer, 'run', side_effect=run):
            with self.assertRaisesRegex(RuntimeError, 'simulated'):
                installer.migrate_database(self.compose, self.directory, self.path, config, self.port)
        self.assertEqual(installer.database_type(installer.load(self.path)), 'sqlite')
        pending = self.directory / 'docker-compose.migrate.yml'
        self.assertTrue(installer.load(pending)['x-embyrelay-installer']['migration_started'])
        installer.migrate_database(self.compose, self.directory, self.path, config, self.port)
        current = installer.load(self.path)
        self.assertEqual(installer.database_type(current), 'postgres')
        self.assertFalse(pending.exists())
        self.assertEqual(self.sql("SELECT COUNT(*) FROM users WHERE username='admin'"), '1')
        installer.healthy(self.compose, 'postgres')


if __name__ == '__main__':
    unittest.main()
