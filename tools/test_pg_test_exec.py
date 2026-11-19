import os
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
WRAPPER = ROOT / 'tools/pg-test-exec.sh'
DSN = 'postgres://u:p@db.example.test:5432/lane_test?sslmode=disable'
# A psql that records its connection and SQL instead of talking to a server.
FAKE_PSQL = '#!/bin/sh\necho "$*" >> "$PSQL_LOG"\n'


def run_in(directory, dsn, bin_dir, log):
    """Run the wrapper in a package directory, as go test does, with a program that prints the DSN it was given."""
    env = dict(os.environ, PATH=f'{bin_dir}:{os.environ["PATH"]}', PSQL_LOG=str(log))
    env.pop('PAPERBOAT_TEST_DATABASE_DSN', None)
    if dsn is not None:
        env['PAPERBOAT_TEST_DATABASE_DSN'] = dsn
    return subprocess.run([WRAPPER, 'sh', '-c', 'echo "$PAPERBOAT_TEST_DATABASE_DSN"'], cwd=directory, env=env,
                          capture_output=True, text=True)


class PgTestExecTests(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(subprocess.run, ['rm', '-rf', str(self.tmp)])
        self.bin = self.tmp / 'bin'
        self.bin.mkdir()
        (self.bin / 'psql').write_text(FAKE_PSQL)
        (self.bin / 'psql').chmod(0o755)
        self.log = self.tmp / 'psql.log'

    def package(self, rel):
        # A module root (go.mod) with the package below it, like server/internal/<package>.
        directory = self.tmp / 'server' / rel
        directory.mkdir(parents=True, exist_ok=True)
        (self.tmp / 'server/go.mod').write_text('module m\n')
        return directory

    def test_two_packages_never_share_a_database(self):
        seen = {}
        for rel in ('internal/httpapi', 'internal/usermachines', 'internal/db'):
            result = run_in(self.package(rel), DSN, self.bin, self.log)
            self.assertEqual(result.returncode, 0, result.stderr)
            seen[rel] = result.stdout.strip()
        self.assertEqual(len(set(seen.values())), len(seen), seen)
        for dsn in seen.values():
            self.assertRegex(dsn, r'^postgres://u:p@db\.example\.test:5432/lane_internal_\w+_test\?sslmode=disable$')
        # Each database is dropped and recreated before its package runs.
        log = self.log.read_text()
        self.assertEqual(log.count('create database'), 3, log)
        self.assertIn('lane_internal_httpapi_test', log)

    def test_long_package_paths_stay_distinct_within_the_identifier_limit(self):
        names = set()
        for rel in ('internal/' + 'a' * 40 + '/one', 'internal/' + 'a' * 40 + '/two'):
            result = run_in(self.package(rel), DSN, self.bin, self.log)
            name = result.stdout.strip().split('/')[-1].split('?')[0]
            self.assertLessEqual(len(name), 63, name)
            names.add(name)
        self.assertEqual(len(names), 2, names)

    def test_a_dsn_that_is_not_lane_test_is_refused_not_shared(self):
        # Before: ${base%%lane_test*} left the whole DSN as the maintenance URL and every package ran on one database.
        result = run_in(self.package('internal/db'), 'postgres://u:p@db.example.test/paperboat_test?sslmode=disable',
                        self.bin, self.log)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('lane_test', result.stderr)
        self.assertFalse(self.log.exists())

    def test_without_a_dsn_it_just_runs_the_binary(self):
        result = run_in(self.package('internal/db'), None, self.bin, self.log)
        self.assertEqual((result.returncode, result.stdout.strip()), (0, ''))

    def test_every_server_test_entry_point_goes_through_the_wrapper(self):
        for directory, target in (('server', 'test'), ('server', 'race')):
            out = subprocess.run(['make', '-n', '-C', str(ROOT / directory), target], capture_output=True, text=True, check=True).stdout
            self.assertIn('tools/pg-test-exec.sh', out, (directory, target))
        root = subprocess.run(['make', '-n', '-C', str(ROOT), 'test'], capture_output=True, text=True, check=True).stdout
        for module in ('server', 'edge'):
            line = next(l for l in root.splitlines() if l.startswith(f'cd {module} '))
            self.assertIn('tools/pg-test-exec.sh', line)
        self.assertIn('pg-test-exec.sh', (ROOT / 'tools/gate.sh').read_text())


if __name__ == '__main__':
    unittest.main()
