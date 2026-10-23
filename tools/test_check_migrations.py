import unittest

from check_migrations import check

ADD = '-- +goose Up\nALTER TABLE paperboat.x ADD COLUMN y text;\n-- +goose Down\nALTER TABLE paperboat.x DROP COLUMN y;\n'
DROP = '-- +goose Up\nALTER TABLE paperboat.x DROP COLUMN y;\n-- +goose Down\nSELECT 1;\n'


class CheckMigrationsTest(unittest.TestCase):
    def test_expand_passes_and_drop_in_down_is_ignored(self):
        self.assertEqual(check('170_add.sql', ADD, {150}), [])

    def test_drop_column_without_marker_is_rejected(self):
        self.assertEqual(len(check('170_drop.sql', DROP, {150})), 1)

    def test_drop_column_after_a_deployed_expand_passes(self):
        self.assertEqual(check('170_drop.sql', '-- contract-after: 150\n' + DROP, {150}), [])

    def test_drop_column_in_the_same_release_as_its_expand_is_rejected(self):
        self.assertIn('not in the deployed server', check('170_drop.sql', '-- contract-after: 169\n' + DROP, {150})[0])

    def test_rename_type_change_and_not_null_are_contract_steps(self):
        for stmt in ['ALTER TABLE a RENAME COLUMN b TO c;', 'ALTER TABLE a RENAME TO b;',
                     'ALTER TABLE a ALTER COLUMN b TYPE bigint;', 'ALTER TABLE a ALTER COLUMN b SET NOT NULL;', 'DROP TABLE a;']:
            self.assertTrue(check('x.sql', '-- +goose Up\n' + stmt + '\n', None), stmt)

    def test_plain_index_needs_concurrently_or_small_table(self):
        plain = '-- +goose Up\nCREATE INDEX i ON paperboat.x(y);\n'
        self.assertEqual(len(check('x.sql', plain, None)), 1)
        self.assertEqual(check('x.sql', '-- small-table\n' + plain, None), [])
        self.assertEqual(check('x.sql', '-- +goose Up\nCREATE INDEX CONCURRENTLY i ON paperboat.x(y);\n', None), [])


if __name__ == '__main__':
    unittest.main()
