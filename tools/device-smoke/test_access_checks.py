import json
from pathlib import Path
import unittest

import access_checks as a
import billing_checks as b
import fixture_checks
import ip_rules_rows
import preflight_fixtures

HERE = Path(__file__).resolve().parent


class Verdicts(unittest.TestCase):
    def setUp(self):
        self.kept = []
        keep, fixture_checks.keep = fixture_checks.keep, lambda check, text: self.kept.append(text) or 'LOG'
        self.addCleanup(setattr, fixture_checks, 'keep', keep)

    def test_a_line_script_passes_only_with_exit_0_no_fail_and_enough_steps(self):
        two = 'PASS one\nPASS two\n'
        self.assertTrue(a.line_verdict('c', 0, two, 's', 2)[0])
        self.assertFalse(a.line_verdict('c', 0, two, 's', 3)[0], 'a run that stopped early printed too few steps')
        self.assertFalse(a.line_verdict('c', 1, two, 's', 2)[0])
        ok, detail = a.line_verdict('c', 0, two + 'FAIL three: why\n', 's', 2)
        self.assertFalse(ok)
        self.assertIn('three: why', detail)

    def test_a_token_secret_never_reaches_the_receipt_or_the_log(self):
        ok, detail = a.line_verdict('c', 1, 'FAIL create: pbat_abc123_DEF\n', 's', 1)
        self.assertNotIn('pbat_abc123', detail + self.kept[-1])

    def test_a_report_script_needs_every_step_to_pass(self):
        report = {'status': 'PASS', 'steps': [{'step': 'a', 'status': 'PASS'}]}
        self.assertTrue(a.report_verdict('c', 0, json.dumps(report), '', 's')[0])
        report['steps'].append({'step': 'b', 'status': 'FAIL', 'detail': 'got 200'})
        ok, detail = a.report_verdict('c', 0, json.dumps(report), '', 's')
        self.assertFalse(ok)
        self.assertIn('got 200', detail)
        self.assertFalse(a.report_verdict('c', 0, 'no json', 'boom', 's')[0])
        self.assertFalse(a.report_verdict('c', 0, json.dumps({'status': 'PASS', 'steps': []}), '', 's')[0])


class EdgeRuleTiming(unittest.TestCase):
    def run_probe(self, answers, seconds, cost=1.0):
        now = [0.0]

        def probe(ip):
            now[0] += cost  # a slow observer: each answer costs `cost` seconds
            return (answers(now[0]), None)

        return ip_rules_rows.stable_on_each(['a'], probe, 200, seconds, stable=8, interval=0, clock=lambda: now[0])[0]

    def test_a_rule_that_applied_in_time_is_not_failed_by_a_slow_observer(self):
        self.assertEqual(self.run_probe(lambda t: 200 if t > 6 else 403, 15, cost=3.0), {'a': 6.0})

    def test_a_rule_that_applies_after_the_bound_fails(self):
        self.assertEqual(self.run_probe(lambda t: 200 if t > 20 else 403, 15), {})

    def test_a_flapping_answer_never_counts(self):
        self.assertEqual(self.run_probe(lambda t: 200 if int(t) % 5 else 403, 15), {})


class TopupSpend(unittest.TestCase):
    def rows(self, remaining, throttled=False, purchased=200):
        before = {'topup_bytes_remaining': 200_000_000_000, 'topup_bytes_purchased': 200}
        after = {'topup_bytes_remaining': remaining, 'topup_bytes_purchased': purchased, 'throttled': throttled}
        return [ok for _, ok, _ in b.spend_rows(before, after, 1_500_000)]

    def test_the_topup_pays_for_the_bytes_sent_and_nothing_wild(self):
        self.assertEqual(self.rows(200_000_000_000 - 3_100_000), [True, True, True])
        self.assertEqual(self.rows(200_000_000_000)[0], False, 'nothing was debited')
        self.assertEqual(self.rows(100_000_000_000)[0], False, 'far more than was sent')
        self.assertEqual(self.rows(200_000_000_000 - 3_100_000, throttled=True)[2], False)


class Registration(unittest.TestCase):
    def test_every_check_runs_in_smoke_and_is_preflighted(self):
        smoke = (HERE / 'smoke.sh').read_text()
        listed = smoke.split('fixture_ids=$(for id in ')[1].split(';')[0].split()
        for check in a.CHECKS:
            self.assertIn('\nfixture_check %s\n' % check, smoke)
            self.assertIn(check, listed)
            self.assertIn(check, preflight_fixtures.NEEDS)
            for need in preflight_fixtures.NEEDS[check]:
                self.assertIn(need, preflight_fixtures.VERIFIERS)


if __name__ == '__main__':
    unittest.main()
