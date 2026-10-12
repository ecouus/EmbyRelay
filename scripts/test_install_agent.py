"""Run with python3 -m unittest discover -s scripts -p 'test_*.py'. No Docker mutations."""
import base64
import contextlib
import io
import json
from pathlib import Path
import subprocess
import tempfile
import types
import unittest
from unittest.mock import patch

source = Path(__file__).with_name('install-agent.sh').read_text().split("<<'PY'\n", 1)[1].rsplit('\nPY', 1)[0]
installer = types.ModuleType('installer')
exec(compile(source, 'install-agent.sh', 'exec'), installer.__dict__)
KEY = base64.b64encode(bytes(32)).decode().rstrip('=')


class InstallerTests(unittest.TestCase):
    def run_install(self, directory, mode='nginx', fail_health=False, token='test-token'):
        argv = ['installer', '--directory', str(directory), '--control-url', 'https://panel.example.com',
                '--domain', 'node.example.com', '--name', 'B $literal', '--server-key', KEY,
                '--token', token, '--mode', mode]
        calls = []
        def run(command, **kwargs):
            calls.append(command)
            return subprocess.CompletedProcess(command, 1 if fail_health and 'exec' in command else 0)
        with patch.object(installer.sys, 'argv', argv), patch.object(installer.platform, 'system', return_value='Linux'), \
             patch.object(installer.subprocess, 'run', side_effect=run), patch.object(installer, 'available_ports'), \
             patch.object(installer, 'setup_nginx') as setup, patch.object(installer.os, 'geteuid', return_value=0), patch.object(installer.time, 'sleep'), contextlib.redirect_stdout(io.StringIO()):
            installer.main()
            setup.assert_called_once()
        return calls

    def test_install_and_reuse_identity(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'docker-compose.yml'
            self.run_install(tmp)
            config = json.loads(path.read_text())
            self.assertEqual(config['services']['agent']['environment']['RELAY_ENROLL_TOKEN'], '')
            self.assertEqual(config['services']['agent']['environment']['RELAY_NODE_NAME'], 'B $$literal')
            self.assertEqual(list(config['services']), ['agent'])
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            before = path.read_text()
            self.run_install(tmp, token='another-token')
            self.assertEqual(before, path.read_text())

    def test_nginx_host_configuration(self):
        with tempfile.TemporaryDirectory() as tmp:
            calls = self.run_install(tmp)
            config = json.loads((Path(tmp) / 'docker-compose.yml').read_text())
            self.assertNotIn('caddy', config['services'])
            self.assertEqual(config['services']['agent']['network_mode'], 'host')
            self.assertNotIn('ports', config['services']['agent'])
            self.assertEqual(config['services']['agent']['environment']['RELAY_LISTEN'], '127.0.0.1:6689')
            self.assertTrue(all(command[0] == 'docker' for command in calls))

    def test_proxy_logic_matches_main_installer(self):
        main_source = Path(__file__).with_name('install.sh').read_text().split("<<'PY'\n", 1)[1].rsplit('\nPY', 1)[0]
        main = types.ModuleType('main_installer')
        exec(compile(main_source, 'install.sh', 'exec'), main.__dict__)
        self.assertEqual(installer.NGINX_TEMPLATE, main.NGINX_TEMPLATE)
        for name in ['nginx_tokens', 'server_blocks', 'names', 'active_nginx_files',
                     'normalized', 'nginx_plan', 'apply_nginx', 'ensure_nginx_certbot']:
            self.assertEqual(getattr(installer, name).__code__.co_code, getattr(main, name).__code__.co_code)
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'site.conf'
            path.write_text(installer.NGINX_TEMPLATE)
            self.assertEqual(installer.nginx_plan('relay.example.com', [path]), ({}, True))

    def test_old_container_proxy_is_not_modified(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'docker-compose.yml'
            path.write_text(json.dumps({'x-embyrelay-installer': {'version': 2, 'mode': 'legacy'}}))
            before = path.read_text()
            with self.assertRaisesRegex(ValueError, '旧版容器反代'):
                self.run_install(tmp)
            self.assertEqual(path.read_text(), before)

    def test_failed_registration_preserves_config_then_accepts_fresh_token(self):
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaisesRegex(ValueError, '尚未就绪'):
                self.run_install(tmp, fail_health=True)
            path = Path(tmp) / 'docker-compose.yml'
            self.assertEqual(json.loads(path.read_text())['services']['agent']['environment']['RELAY_ENROLL_TOKEN'], 'test-token')
            self.run_install(tmp, token='fresh-token')
            self.assertEqual(json.loads(path.read_text())['services']['agent']['environment']['RELAY_ENROLL_TOKEN'], '')

    def test_unmanaged_compose_is_untouched(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'docker-compose.yml'
            path.write_text('services: {}\n')
            with self.assertRaisesRegex(ValueError, '不是本脚本'):
                self.run_install(tmp)
            self.assertEqual(path.read_text(), 'services: {}\n')

    def test_invalid_inputs(self):
        for value in ['http://panel.example.com', 'https://panel.example.com/path', 'https://user:pass@panel.example.com', 'https://panel.example.com?token=x']:
            with self.assertRaises(ValueError):
                installer.origin(value)
        for value in ['https://node.example.com', 'node.example.com:443', 'node.example.com\nmalicious', '127.0.0.1', '*.example.com']:
            with self.assertRaises(ValueError):
                installer.domain(value)

    def test_busy_port_rejected(self):
        with patch.object(installer.socket, 'socket') as socket:
            socket.return_value.__enter__.return_value.bind.side_effect = OSError('in use')
            with self.assertRaisesRegex(ValueError, '端口 6689'):
                installer.available_ports('nginx')


if __name__ == '__main__':
    unittest.main()
