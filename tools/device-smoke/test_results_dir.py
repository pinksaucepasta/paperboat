"""Every receipt written by a device-smoke script goes through results_dir(), so SMOKE_RESULTS_DIR is honoured."""
from pathlib import Path
import re
import unittest

HERE = Path(__file__).parent
# A hardcoded results/ path: a '/' join or a join() on 'results'.
HARDCODED = re.compile(r"""/\s*['"]results['"]|join\([^)]*['"]results['"]""")
# Read-only uses of the default results/ directory (config fallback, latest-receipt lookup).
READS = {'dev_targets.py', 'green_list.py', 'last_green.py', 'release_run.py', 'results_dir.py'}


class ResultsDirTests(unittest.TestCase):
    def test_no_script_hardcodes_a_results_path(self):
        offenders = []
        for path in sorted(HERE.iterdir()):
            if path.suffix not in {'.py', '.sh', '.mjs'} or path.name.startswith('test_') or path.name in READS:
                continue
            for number, line in enumerate(path.read_text().splitlines(), 1):
                if HARDCODED.search(line) and 'results_dir(' not in line:
                    offenders.append(f'{path.name}:{number}: {line.strip()}')
        self.assertEqual(offenders, [], 'write receipts with results_dir(__file__), not a hardcoded results/ path')


if __name__ == '__main__':
    unittest.main()
