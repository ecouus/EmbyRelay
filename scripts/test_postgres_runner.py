"""Check that PostgreSQL test selection cannot silently skip its release gate."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

RUNNER = Path(__file__).with_name('test-postgres.sh')


class PostgresRunnerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        self.log = root / 'calls.jsonl'
        self.env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ['PATH'],
                        ER_RUNNER_LOG=str(self.log), ER_RUNNER_PATH=str(RUNNER))
        self.env.pop('EMBYRELAY_TEST_POSTGRES_URL', None)
        go = root / 'go'
        go.write_text('''#!/usr/bin/env python3
import json, os, subprocess, sys
with open(os.environ['ER_RUNNER_LOG'], 'a') as log:
    log.write(json.dumps(sys.argv[1:]) + '\\n')
if '-list' in sys.argv:
    names = subprocess.check_output(['bash', os.environ['ER_RUNNER_PATH'], '--list'], text=True).splitlines()
    for name in names:
        if name != os.getenv('ER_MISSING_TEST'):
            print(name)
else:
    sys.exit(int(os.getenv('ER_TEST_EXIT', '0')))
''')
        go.chmod(0o755)

    def run_script(self, *args):
        return subprocess.run(['bash', str(RUNNER), *args], env=self.env,
                              text=True, capture_output=True)

    def calls(self):
        if not self.log.exists():
            return []
        return [json.loads(line) for line in self.log.read_text().splitlines()]

    def test_missing_database_rejects_focused_and_full(self):
        for mode in ['--focused', '--full']:
            self.assertEqual(self.run_script(mode).returncode, 2)
        self.assertEqual(self.calls(), [])
        result = self.run_script('--list')
        self.assertEqual(result.returncode, 0)
        self.assertIn('TestHistoricalLoginCollisionStopsMigration', result.stdout)
        self.assertEqual(self.calls(), [])

    def test_renamed_selected_test_blocks_execution(self):
        self.env['EMBYRELAY_TEST_POSTGRES_URL'] = 'postgres://disposable.test/test'
        self.env['ER_MISSING_TEST'] = 'TestUsageRetentionSchemaUpgrade'
        result = self.run_script()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Selected PostgreSQL test is missing', result.stderr)
        self.assertEqual(len(self.calls()), 1)

    def test_focused_selection_is_exact_and_propagates_failure(self):
        self.env['EMBYRELAY_TEST_POSTGRES_URL'] = 'postgres://disposable.test/test'
        self.env['ER_TEST_EXIT'] = '7'
        result = self.run_script()
        self.assertEqual(result.returncode, 7)
        command = self.calls()[-1]
        self.assertIn('-race', command)
        pattern = command[command.index('-run') + 1]
        import re
        self.assertIsNotNone(re.fullmatch(pattern, 'TestDatabaseCopyRoundTripAndResume'))
        self.assertIsNone(re.fullmatch(pattern, 'TestDatabaseCopyRoundTripAndResumeUnselected'))
        self.assertIsNone(re.fullmatch(pattern, 'TestVerifyEmbySource'))

    def test_full_mode_has_no_selection_filter(self):
        self.env['EMBYRELAY_TEST_POSTGRES_URL'] = 'postgres://disposable.test/test'
        result = self.run_script('--full')
        self.assertEqual(result.returncode, 0)
        command = self.calls()[0]
        self.assertNotIn('-run', command)
        self.assertIn('-race', command)
        self.assertEqual(command[-3:], ['./internal/control', './internal/database', './migrations'])
