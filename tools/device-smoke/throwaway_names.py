"""Machine-name prefixes of throwaway test machines: the one list cleanup_test_account.py revokes from."""
import re

# Run prefixes the check scripts give their throwaway machines (`<prefix><digits-or-words>`).
PREFIXES = ('test-', 'fx-', 'sh-r1-', 'sh-slr-', 'sh-probe-', 'pbt1-')
# Older families that are regexes, not plain prefixes.
PATTERNS = (r'worktree[a-z0-9]*-', r'luf14', r'winpool-', r'linuxpool-', r'macpool-', r'winrel', r'wina-', r'w[0-9]+-')


def throwaway_regex(extra=None):
    """One anchored regex: every shared prefix/pattern, plus a configured extra (always unioned, never replacing)."""
    parts = list(PATTERNS) + [re.escape(p) for p in PREFIXES]
    if extra:
        parts.append('(?:' + extra + ')')
    return '^(' + '|'.join(parts) + ')'
