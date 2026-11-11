import importlib.util
import unittest
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
    def test_database_dsn_keeps_the_server_and_changes_the_name(self):
        self.assertEqual(checks.database_dsn('postgres://u:p@localhost:5432/paperboat_test?sslmode=disable', 'paperboat_pr_3'),
                         'postgres://u:p@localhost:5432/paperboat_pr_3?sslmode=disable')

    def test_database_names_satisfy_the_servers_isolation_guard(self):
        for index in (0, 7, 31):
            self.assertTrue(checks.database_name(index).endswith('_test'))
