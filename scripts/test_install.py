"""Installer lifecycle tests use temporary files and mocked Docker; no deployment."""
import contextlib
import io
import json
from pathlib import Path
import subprocess
import tempfile
import types
import unittest
from unittest.mock import patch

source = Path(__file__).with_name('install.sh').read_text().split("<<'PY'\n", 1)[1].rsplit('\nPY', 1)[0]
installer = types.ModuleType('main_installer')
exec(compile(source, 'install.sh', 'exec'), installer.__dict__)


class MainInstallerTests(unittest.TestCase):
    def config(self):
        return installer.configuration('relay.example.com', 'admin', 'p$a${ss}word\\"')

    def postgres_config(self):
        return installer.configuration('relay.example.com', 'admin', 'password', 'postgres', 'x' * 48)

    def test_postgres_configuration_and_secrets(self):
        config = self.postgres_config()
        pg = config['services']['postgres']
        app = config['services']['embyrelay']
        self.assertEqual(pg['ports'], ['127.0.0.1:15432:5432'])
        self.assertEqual(app['environment']['RELAY_DB_DRIVER'], 'postgres')
        self.assertEqual(app['depends_on']['postgres']['condition'], 'service_healthy')
        self.assertNotIn('x' * 48, json.dumps(config))
        self.assertIn('postgres_data', config['volumes'])
        with tempfile.TemporaryDirectory() as temp, patch.object(installer.os, 'chown') as chown:
            installer.save_database_secrets(Path(temp), 'x' * 48, 15432)
            secret = Path(temp) / 'secrets/database_url'
            self.assertEqual(secret.stat().st_mode & 0o777, 0o400)
            chown.assert_called_once_with(secret, 10001, 10001)
            self.assertIn('@127.0.0.1:15432/', secret.read_text())

    def test_old_configuration_defaults_to_sqlite(self):
        config = self.config()
        del config['x-embyrelay-installer']['database']
        self.assertEqual(installer.database_type(config), 'sqlite')

    def test_old_image_cannot_silently_use_sqlite_in_postgres_mode(self):
        result = types.SimpleNamespace(returncode=0, stdout='{"status":"ok"}')
        with patch.object(installer.subprocess, 'run', return_value=result), patch.object(installer, 'run') as run:
            with self.assertRaisesRegex(ValueError, '镜像未启用'):
                installer.healthy(['compose'], 'postgres')
            run.assert_called_once_with(['compose', 'stop', 'embyrelay'])

    def test_postgres_backup_contains_database_and_master_key(self):
        config = self.postgres_config()
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp)
            installer.save(directory / 'docker-compose.yml', config)
            with patch.object(installer.os, 'chown'):
                installer.save_database_secrets(directory, 'x' * 48, 15432)
            calls = []
            def run(command, **kwargs):
                calls.append(command)
                if 'stdout' in kwargs:
                    kwargs['stdout'].write(b'data')
            with patch.object(installer, 'run', side_effect=run):
                installer.backup(['compose'], directory, config)
            archive = next((directory / 'backups').glob('*.tar.gz'))
            with installer.tarfile.open(archive) as saved:
                self.assertIn('database.dump', saved.getnames())
                self.assertIn('data.tar.gz', saved.getnames())
                self.assertIn('secrets/database_url', saved.getnames())
            self.assertEqual(archive.stat().st_mode & 0o777, 0o600)
            self.assertEqual(calls[-1], ['compose', 'start', 'embyrelay'])

    def test_restore_rejects_path_traversal(self):
        with tempfile.TemporaryDirectory() as temp:
            archive = Path(temp) / 'bad.tar.gz'
            with installer.tarfile.open(archive, 'w:gz') as saved:
                member = installer.tarfile.TarInfo('../evil')
                member.size = 1
                saved.addfile(member, io.BytesIO(b'x'))
            with self.assertRaisesRegex(ValueError, '不安全'):
                installer.validate_data_archive(archive)

    def test_delete_removes_owned_secrets_directory(self):
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp)
            installer.save(directory / 'docker-compose.yml', self.postgres_config())
            with patch.object(installer.os, 'chown'):
                installer.save_database_secrets(directory, 'x' * 48, 15432)
            with patch.object(installer, 'ask', side_effect=['yes', 'DELETE']), patch.object(installer, 'run'):
                installer.uninstall(['compose'], directory)
            self.assertFalse((directory / 'secrets').exists())
            self.assertFalse((directory / 'docker-compose.yml').exists())

    def test_host_network_and_trust_boundary(self):
        config = self.config()
        for service in config['services'].values():
            self.assertEqual(service['network_mode'], 'host')
            self.assertNotIn('ports', service)
        env = config['services']['embyrelay']['environment']
        self.assertEqual(env['RELAY_LISTEN'], '127.0.0.1:6689')
        self.assertEqual(env['RELAY_TRUSTED_PROXIES'], '127.0.0.1/32')
        self.assertEqual(env['RELAY_ALLOW_INSECURE'], 'false')
        self.assertEqual(list(config['services']), ['embyrelay'])
        self.assertIn('proxy_pass http://127.0.0.1:6689;', installer.NGINX_TEMPLATE)

    def test_nginx_reuses_readme_template(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / 'site.conf'
            path.write_text(installer.NGINX_TEMPLATE)
            edits, correct = installer.nginx_plan('relay.example.com', [path])
            self.assertTrue(correct)
            self.assertEqual(edits, {})

    def test_nginx_preserves_other_sites_and_replaces_wrong_proxy(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / 'site.conf'
            unrelated = 'server { listen 80; server_name other.example.com; return 200 "ok"; }'
            path.write_text(unrelated + '\nserver { listen 80; server_name relay.example.com; proxy_pass http://wrong; }')
            edits, correct = installer.nginx_plan('relay.example.com', [path])
            self.assertFalse(correct)
            self.assertEqual(edits[path].strip(), unrelated)
            generated = next(v for k,v in edits.items() if k != path)
            self.assertIn('proxy_pass http://127.0.0.1:6689;', generated)
            self.assertIn('proxy_set_header X-Forwarded-For   $remote_addr;', generated)

    def test_nginx_shared_domain_block_is_not_overwritten(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / 'site.conf'
            path.write_text('server { server_name relay.example.com other.example.com; }')
            with self.assertRaisesRegex(ValueError, '共用'):
                installer.nginx_plan('relay.example.com', [path])

    def test_nginx_rollback_on_invalid_config(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / 'site.conf'
            path.write_text('original')
            with patch.object(installer, 'run', side_effect=[ValueError('invalid'), None, None]):
                with self.assertRaises(ValueError):
                    installer.apply_nginx({path: 'changed'}, Path(temp))
            self.assertEqual(path.read_text(), 'original')
            self.assertTrue(list((Path(temp) / 'backups').glob('nginx-*/0')))

    def test_generated_config_is_idempotent(self):
        host = 'relay.example.com'
        target = Path('/etc/nginx/conf.d/embyrelay-' + host + '.conf')
        variable = 'embyrelay_connection_' + installer.hashlib.sha256(host.encode()).hexdigest()[:12]
        content = installer.NGINX_TEMPLATE.replace('$embyrelay_connection', '$' + variable)
        with patch.object(Path, 'read_text', return_value=content), patch.object(Path, 'exists', return_value=True):
            self.assertEqual(installer.nginx_plan(host, [target]), ({}, True))

    def test_nginx_template_matches_readme(self):
        readme = Path(__file__).resolve().parents[1].joinpath('README.md').read_text()
        template = readme.split('```nginx\n', 1)[1].split('```', 1)[0]
        self.assertEqual(installer.NGINX_TEMPLATE, template)

    def test_existing_packages_are_not_installed_again(self):
        def run(command, **kwargs):
            self.assertNotIn('apt-get', command)
            return subprocess.CompletedProcess(command, 0, '* nginx', '')
        with patch.object(installer.shutil, 'which', side_effect=lambda n: '/usr/bin/' + n), patch.object(installer, 'run', side_effect=run):
            installer.ensure_nginx_certbot()

    def test_domain_rejects_config_injection(self):
        for value in ['example.com {', 'https://example.com', '*.example.com', 'example.com:443',
                      '127.0.0.1', '-bad.com', 'a..com', 'a.com\nlog', 'localhost']:
            with self.subTest(value=value), self.assertRaises(ValueError):
                installer.domain(value)
        self.assertEqual(installer.domain('Relay.Example.COM.'), 'relay.example.com')

    def test_compose_secret_roundtrip_and_permissions(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / 'docker-compose.yml'
            config = self.config()
            installer.save(path, config)
            self.assertIn('p$$a$${ss}', path.read_text())
            self.assertEqual(installer.load(path), config)
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            path.write_text('{"services": {}}')
            with self.assertRaises(ValueError):
                installer.load(path)

    def test_bootstrap_cleanup_only_after_health(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / 'docker-compose.yml'
            config = self.config()
            installer.save(path, config)
            with patch.object(installer, 'run') as run, patch.object(installer, 'healthy', side_effect=ValueError('down')):
                with self.assertRaises(ValueError):
                    installer.start(['compose'], path, config)
            self.assertEqual(installer.load(path), self.config())
            with patch.object(installer, 'run') as run, patch.object(installer, 'healthy') as health:
                installer.start(['compose'], path, config)
            self.assertEqual(installer.load(path)['services']['embyrelay']['environment']['RELAY_ADMIN_PASSWORD'], '')
            self.assertEqual(health.call_count, 2)
            self.assertIn(unittest.mock.call(['compose', 'up', '-d', '--force-recreate', 'embyrelay']), run.call_args_list)

    def test_reset_secret_uses_stdin_not_arguments(self):
        with patch.object(installer, 'run') as run:
            installer.reset_password(['compose'], 'admin', 'pa$$word')
        command = run.call_args.args[0]
        self.assertNotIn('pa$$word', ' '.join(command))
        self.assertEqual(run.call_args.kwargs['input'], 'pa$$word\n')
        self.assertIn('"$1"', command[-3])

    def test_uninstall_retains_data_by_default_and_requires_exact_delete(self):
        for answer, deleted in [('', False), ('yes', False), ('DELETE', True)]:
            with self.subTest(answer=answer), tempfile.TemporaryDirectory() as temp:
                directory = Path(temp)
                for name in ['docker-compose.yml', 'Caddyfile', 'unrelated']:
                    (directory / name).write_text('keep')
                with patch.object(installer, 'ask', side_effect=['yes', answer]), \
                     patch.object(installer, 'run') as run, contextlib.redirect_stdout(io.StringIO()):
                    installer.uninstall(['compose'], directory)
                self.assertEqual('--volumes' in run.call_args.args[0], deleted)
                self.assertEqual((directory / 'docker-compose.yml').exists(), not deleted)
                self.assertTrue((directory / 'unrelated').exists())
        with patch.object(installer, 'ask', return_value='no'), patch.object(installer, 'run') as run:
            installer.uninstall(['compose'], Path('/not-used'))
            run.assert_not_called()

    def test_backup_failure_restarts_backend_and_removes_incomplete_archive(self):
        with tempfile.TemporaryDirectory() as temp:
            calls = []
            def run(command, **kwargs):
                calls.append(command)
                if 'tar' in command:
                    raise subprocess.CalledProcessError(1, command)
            with patch.object(installer, 'run', side_effect=run), self.assertRaises(subprocess.CalledProcessError):
                installer.backup(['compose'], Path(temp))
            self.assertEqual(calls[0], ['compose', 'stop', 'embyrelay'])
            self.assertEqual(calls[-1], ['compose', 'start', 'embyrelay'])
            self.assertEqual(list((Path(temp) / 'backups').iterdir()), [])

    def test_https_timeout_does_not_claim_certificate_success(self):
        with patch.object(installer.time, 'monotonic', side_effect=[0, 100]), \
             contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertFalse(installer.check_https('relay.example.com'))
        self.assertIn('尚未验证通过', output.getvalue())

    def test_prune_only_automatic_backups(self):
        with tempfile.TemporaryDirectory() as temp:
            folder = Path(temp) / 'backups'
            folder.mkdir()
            for index in range(7):
                (folder / f'auto-{index}.tar.gz').write_bytes(b'backup')
            (folder / 'manual.tar.gz').write_bytes(b'keep')
            installer.prune_automatic_backups(Path(temp), keep=5)
            self.assertEqual(len(list(folder.glob('auto-*.tar.gz'))), 5)
            self.assertTrue((folder / 'manual.tar.gz').exists())


if __name__ == '__main__':
    unittest.main()
