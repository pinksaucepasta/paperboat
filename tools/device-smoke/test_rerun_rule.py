import contextlib
import io
import json
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

import rerun_rule

HERE = Path(__file__).resolve().parent


class Verdicts(unittest.TestCase):
    def test_a_row_that_failed_then_passed_is_a_fail_until_its_cause_is_known(self):
        self.assertEqual(rerun_rule.verdict(['FAIL', 'PASS']), ('FAIL', 'FAIL (passed on rerun, cause unknown)'))
        self.assertEqual(rerun_rule.verdict(['FAIL', 'NOT RUN', 'PASS'])[0], 'FAIL')

    def test_only_a_script_or_runner_cause_makes_it_a_pass_and_product_stays_fail(self):
        for cls, state in (('script', 'PASS'), ('runner', 'PASS'), ('product', 'FAIL')):
            self.assertEqual(rerun_rule.verdict(['FAIL', 'PASS'], (cls, 'why')),
                             (state, '%s (passed on rerun, %s: why)' % (state, cls)))

    def test_a_cause_never_turns_a_row_that_did_not_pass_into_a_pass(self):
        for states in (['FAIL'], ['PASS', 'FAIL'], ['FAIL', 'SKIP'], ['FAIL', 'NOT RUN']):
            self.assertEqual(rerun_rule.verdict(states, ('script', 'why'))[0], 'FAIL', states)

    def test_rows_that_never_failed_keep_their_result(self):
        for states in (['PASS'], ['SKIP'], ['NOT RUN'], ['NOT RUN', 'PASS']):
            self.assertEqual(rerun_rule.verdict(states), (states[-1], states[-1]))

    def test_the_cause_file(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'causes.tsv'
            self.assertEqual(rerun_rule.load_causes(path), {})
            path.write_text('# comment\n\nexec:a\tscript\tbad quoting\nLN2/ssh:a\trunner\tlease ended\n')
            causes = rerun_rule.load_causes(path)
            self.assertEqual(rerun_rule.cause_for(causes, 'LN1', 'exec:a'), ('script', 'bad quoting'))
            self.assertEqual(rerun_rule.cause_for(causes, 'LN2', 'ssh:a'), ('runner', 'lease ended'))
            self.assertIsNone(rerun_rule.cause_for(causes, 'LN1', 'ssh:a'))
            for bad in ('exec:a script x', 'exec:a\tflaky\tx', 'exec:a\tscript\t', 'a\tscript\tx\na\tproduct\ty'):
                path.write_text(bad + '\n')
                with self.assertRaises(ValueError, msg=bad):
                    rerun_rule.load_causes(path)


class SmokeReport(unittest.TestCase):
    """smoke.sh's own report, run with the function as it is in the script."""

    def report(self, earlier, rows, causes=''):
        function = re.search(r'^apply_rerun_rule\(\) \{\n.*?^\}\n', (HERE / 'smoke.sh').read_text(), re.S | re.M).group(0)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for number, (version, results) in enumerate(earlier):
                (root / ('%d.json' % number)).write_text(json.dumps({'expect_version': version, 'results': [
                    {'id': key, 'status': value} for key, value in results.items()]}))
            (root / 'causes.tsv').write_text(causes)
            script = ('set -u\nHERE=%s RESULTS_DIR=%s EXPECT_VERSION=v2 RUN_ID=smoke1 SMOKE_RUN_ID=LN1 SMOKE_RERUN_CAUSES=%s/causes.tsv FAILED=0\n'
                      'ROWS=(%s)\n%sapply_rerun_rule\nprintf "ROW %%s\\n" "${ROWS[@]}"\necho FAILED=$FAILED\n'
                      % (HERE, root, root, ' '.join("'%s|%s|1||'" % row for row in rows), function))
            return subprocess.run(['bash', '-c', script], capture_output=True, text=True, timeout=60, check=True).stdout

    def test_a_rerun_pass_is_reported_and_stored_as_fail(self):
        out = self.report([('v2', {'a:x': 'FAIL'}), ('v1', {'b:x': 'FAIL'})], [('a:x', 'PASS'), ('b:x', 'PASS')])
        self.assertIn('RERUN a:x: FAIL (passed on rerun, cause unknown)', out)
        self.assertIn('ROW a:x|FAIL|1|FAIL (passed on rerun, cause unknown)|', out)
        self.assertIn('ROW b:x|PASS|', out)  # another build's failure is no rerun
        self.assertIn('FAILED=1', out)

    def test_a_recorded_script_cause_annotates_and_a_product_cause_stays_fail(self):
        earlier = [('v2', {'a:x': 'FAIL'})]
        out = self.report(earlier, [('a:x', 'PASS')], 'LN1/a:x\tscript\tfixture\n')
        self.assertIn('ROW a:x|PASS|1|PASS (passed on rerun, script: fixture)|', out)
        self.assertIn('FAILED=0', out)
        out = self.report(earlier, [('a:x', 'PASS')], 'a:x\tproduct\tbug\n')
        self.assertIn('ROW a:x|FAIL|', out)
        self.assertIn('FAILED=1', out)

    def test_a_broken_cause_file_never_passes_the_run(self):
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertIn('FAILED=1', self.report([], [('a:x', 'PASS')], 'a:x\tflaky\tx\n'))

    def test_the_report_applies_the_rule_before_it_writes_the_receipt(self):
        text = (HERE / 'smoke.sh').read_text()
        self.assertLess(text.index('\napply_rerun_rule\n'), text.index('} > "$RESULTS_DIR/$STAMP.json"'))


if __name__ == '__main__':
    unittest.main()
