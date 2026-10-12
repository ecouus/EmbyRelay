"""Exercise installer prompts against a real, non-seekable terminal."""
import builtins
import os
from pathlib import Path
import types
import unittest
from unittest.mock import patch


class TerminalInputTests(unittest.TestCase):
    def load_installer(self, name):
        source = Path(__file__).with_name(name).read_text().split("<<'PY'\n", 1)[1].rsplit('\nPY', 1)[0]
        module = types.ModuleType('terminal_installer')
        exec(compile(source, name, 'exec'), module.__dict__)
        return module

    def test_menu_and_default_with_real_terminal(self):
        for name in ('install.sh', 'install-agent.sh'):
            with self.subTest(installer=name):
                installer = self.load_installer(name)
                master, slave = os.openpty()
                terminal = os.ttyname(slave)
                def terminal_open(path, *args, **kwargs):
                    return builtins.open(terminal if path == '/dev/tty' else path, *args, **kwargs)
                try:
                    os.write(master, b'1\n\n')
                    with patch.object(installer, 'open', side_effect=terminal_open, create=True):
                        self.assertEqual(installer.ask('请选择操作'), '1')
                        self.assertEqual(installer.ask('请选择数据库', '1'), '1')
                finally:
                    os.close(master)
                    os.close(slave)

    def test_password_prompt_uses_writable_terminal(self):
        installer = self.load_installer('install.sh')
        master, slave = os.openpty()
        terminal = os.ttyname(slave)
        def terminal_open(path, *args, **kwargs):
            return builtins.open(terminal if path == '/dev/tty' else path, *args, **kwargs)
        def password_prompt(prompt, stream):
            stream.write(prompt)
            stream.flush()
            return 'test-password-123'
        try:
            with patch.object(installer, 'open', side_effect=terminal_open, create=True), \
                    patch.object(installer.getpass, 'getpass', side_effect=password_prompt):
                self.assertEqual(installer.password(), 'test-password-123')
        finally:
            os.close(master)
            os.close(slave)
