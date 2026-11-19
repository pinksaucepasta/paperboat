import importlib.util
import unittest
from unittest import mock
from pathlib import Path

spec = importlib.util.spec_from_file_location('pr_checks', Path(__file__).with_name('pr-checks.py'))
checks = importlib.util.module_from_spec(spec)
spec.loader.exec_module(checks)


class AffectedTests(unittest.TestCase):
    def test_change_propagates_to_workspace_and_test_importers(self):
        packages = {
            'test/shared/wire': dict(directory='shared/wire', module='shared', module_path='test/shared', imports=[]),
            'test/cli/cmd': dict(directory='cli/cmd', module='cli', module_path='test/cli', imports=['test/shared/wire']),
            'test/server/api': dict(directory='server/api', module='server', module_path='test/server', imports=['test/cli/cmd']),
            'test/relay/other': dict(directory='relay/other', module='relay', module_path='test/relay', imports=[]),
        }
        for paths, expected in [
            (['shared/wire/frame.go'], ['test/cli/cmd', 'test/server/api', 'test/shared/wire']),
            (['cli/cmd/deleted_test.go'], ['test/cli/cmd', 'test/server/api']),
            (['shared/go.sum'], ['test/cli/cmd', 'test/server/api', 'test/shared/wire']),
            (['go.work'], sorted(packages)),
            (['CHECKLIST.md'], []),
            (['cli/cmd/data/fixture.txt'], ['test/cli/cmd', 'test/server/api']),
        ]:
            with self.subTest(paths=paths):
                self.assertEqual(checks.affected(packages, paths), expected)

    def test_removed_package_still_selects_its_importers(self):
        packages = {'test/cli/cmd': dict(directory='cli/cmd', module='cli', module_path='test/cli', imports=['test/cli/removed'])}
        self.assertEqual(checks.affected(packages, ['cli/removed/a.go']), ['test/cli/cmd'])


class DatabaseTests(unittest.TestCase):
    def test_database_modules_run_their_tests_through_the_pg_test_exec_wrapper(self):
        for module, dsn, wrapped in (('server', 'postgres://x/lane_test', True), ('cli', 'postgres://x/lane_test', False),
                                     ('server', None, False)):
            with self.subTest(module=module, dsn=dsn), mock.patch.object(checks.subprocess, 'run') as run, \
                    mock.patch.dict(checks.os.environ, {'PAPERBOAT_TEST_DATABASE_DSN': dsn} if dsn else {}, clear=not dsn):
                checks.test_packages(module, ['p/a'])
                command = run.call_args.args[0]
                self.assertEqual(['-exec', str(checks.ROOT / 'tools/pg-test-exec.sh')] if wrapped else [],
                                 command[command.index('-count=1') + 1:-1])
                self.assertEqual(command[-1], 'p/a')

    def test_fast_mode_selects_only_the_changed_packages(self):
        packages = {
            'm/a': {'directory': 'shared/a', 'module': 'shared', 'module_path': 'm', 'imports': []},
            'm/b': {'directory': 'server/b', 'module': 'server', 'module_path': 'm', 'imports': ['m/a']},
        }
        self.assertEqual(checks.affected(packages, ['shared/a/x.go']), ['m/a', 'm/b'])
        self.assertEqual(checks.affected(packages, ['shared/a/x.go'], transitive=False), ['m/a'])


class FormatTests(unittest.TestCase):
    def test_unformatted_file_anywhere_in_a_module_is_reported_even_if_unchanged(self):
        import tempfile
        with tempfile.TemporaryDirectory() as root:
            for module, text in (('shared/deep/pkg', 'package p\nvar x   = 1\n'), ('cli', 'package p\n\nvar y = 1\n')):
                directory = Path(root, module)
                directory.mkdir(parents=True)
                (directory / 'f.go').write_text(text)
            for module in checks.MODULES:
                Path(root, module).mkdir(parents=True, exist_ok=True)
            self.assertEqual([Path(p).name for p in checks.unformatted(root)], ['f.go'])
            self.assertEqual(checks.unformatted(root)[0], 'shared/deep/pkg/f.go')
