"""throwaway_names: every shared prefix matches, unlisted names do not, a protected name is never selected."""
import re
import unittest

from throwaway_names import PREFIXES, throwaway_regex

PROTECTED = r'^(prod|linux|macos|canary|teammate)'  # stand-in for cleanup_protected; the SQL uses it with !~*


def selected(alias, throwaway, protected=PROTECTED):
    # Mirrors cleanup_test_account.py's SQL: alias ~ throwaway AND alias !~* protected
    return bool(re.search(throwaway, alias)) and not re.search(protected, alias, re.IGNORECASE)


class ThrowawayNamesTest(unittest.TestCase):
    def setUp(self):
        self.regex = throwaway_regex()

    def test_every_prefix_matches(self):
        for prefix in PREFIXES:
            self.assertTrue(prefix.endswith('-'), prefix)
            self.assertTrue(selected(prefix + '123', self.regex), prefix)

    def test_check_names_match(self):
        for name in ('test-pbu1-1795274770-again', 'sh-r1-5', 'pbt1-x', 'test-slr-9', 'test-rc-1-ab', 'worktree2-a', 'w3-x', 'macpool-b1-2', 'winpool-1'):
            self.assertTrue(selected(name, self.regex), name)

    def test_linuxpool_runner_names_match_the_pattern_list(self):
        self.assertTrue(re.search(self.regex, 'linuxpool-b1-3'))

    def test_unlisted_names_do_not_match(self):
        for name in ('linux', 'macos', 'canary-x', 'teammate', 'prod-1', 'sh-other-1'):
            self.assertFalse(selected(name, self.regex), name)

    def test_protected_wins_over_prefix(self):
        self.assertFalse(selected('test-prod-1', self.regex, protected=r'prod'))
        self.assertFalse(selected('sh-r1-linux', self.regex, protected=r'linux'))

    def test_configured_extra_is_unioned_not_replacing(self):
        regex = throwaway_regex('^mine-')
        self.assertTrue(selected('mine-1', regex))
        self.assertTrue(selected('test-1', regex))


if __name__ == '__main__':
    unittest.main()
