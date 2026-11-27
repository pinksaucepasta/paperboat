#!/usr/bin/env python3
"""billing_checks.py, workspace_usage.py and the billing browser scripts' pure parts."""
import json
from pathlib import Path
import shutil
import subprocess
import unittest

import billing_checks as b
import fixture_checks
import preflight_fixtures
import workspace_usage

HERE = Path(__file__).resolve().parent
USER = 'user_' + '0' * 8 + '-0000-0000-0000-' + '0' * 12


def overview(**changes):
    base = {'product': {'interval': 'month'}, 'allowance_period_start': '2026-11-27T05:40:24Z', 'allowance_period_end': '2026-12-27T05:40:24Z',
            'period_start': '2026-11-27T05:40:24Z', 'period_end': '2026-12-27T05:40:24Z', 'allowance_bytes': 100_000_000_000, 'usage_bytes': 7_000_000}
    return {**base, **changes}


class Anchor(unittest.TestCase):
    def failed(self, now, interval):
        return [step for step, ok, _ in b.anchor_rows(overview(), now, interval, 7_000_000) if not ok]

    def test_yearly_keeps_window_day_and_spend(self):
        self.assertEqual(self.failed(overview(product={'interval': 'year'}, period_end='2027-11-27T05:40:24Z'), 'year'), [])
        self.assertEqual(self.failed(overview(), 'month'), [])

    def test_a_reset_window_lost_spend_or_moved_renewal_fails(self):
        moved = overview(product={'interval': 'year'}, period_end='2027-11-27T05:43:00Z', allowance_period_start='2026-11-27T05:43:00Z', usage_bytes=0)
        self.assertEqual(len(self.failed(moved, 'year')), 3)

    def test_a_leap_day_purchase_renews_on_the_28th(self):
        leap = overview(period_start='2028-02-29T05:00:00Z')
        rows = b.anchor_rows(leap, overview(product={'interval': 'year'}, period_end='2029-02-28T05:00:00Z'), 'year', 0)
        self.assertTrue(dict((step, ok) for step, ok, _ in rows)['the renewal keeps the billing day and time'])

    def test_a_switch_that_did_not_happen_fails(self):
        self.assertIn('the plan is billed by the year', self.failed(overview(), 'year'))


class Throttle(unittest.TestCase):
    HUMAN = ("The workspace used its traffic allowance. Transfers through Paperboat relays are throttled to the plan's reduced speed (0.512 Mbit/s shared across relays). "
             'Use a direct connection or your own relay, add bandwidth or change plan in Billing, or wait for the allowance to reset.')
    EVENT = json.dumps({'event': 'transfer_throttled', 'reason': 'allowance_used', 'plan_mbit_per_second': 0.512, 'recovery': 'add bandwidth or change plan in Billing'})

    def failed(self, *args):
        return [step for step, ok, _ in b.throttle_rows(*args) if not ok]

    def test_slowed_send_with_the_notice_passes(self):
        self.assertEqual(self.failed(0, 'Sent', self.HUMAN, 23.0, b.SEND_BYTES, False), [])
        self.assertEqual(self.failed(0, '{"ok":true}', self.EVENT + '\n{"event":"transfer_progress"}', 23.0, b.SEND_BYTES, True), [])

    def test_a_cut_send_a_fast_send_or_a_silent_one_fails(self):
        self.assertIn('pb send is not cut: it exits 0', self.failed(1, '', 'pb: Operation canceled.', 20.0, b.SEND_BYTES, False))
        self.assertIn('no cancellation or quota refusal is shown', self.failed(1, '', 'pb: Operation canceled.', 20.0, b.SEND_BYTES, False))
        self.assertIn('it runs at the slowed speed', self.failed(0, 'Sent', self.HUMAN, 2.0, b.SEND_BYTES, False))
        self.assertEqual(len(self.failed(0, 'Sent', '', 23.0, b.SEND_BYTES, False)), 1)
        self.assertEqual(len(self.failed(0, '{"ok":true}', '', 23.0, b.SEND_BYTES, True)), 1)


class Receipt(unittest.TestCase):
    def test_every_failed_step_is_named_and_none_stops_the_rest(self):
        steps = b.Steps()
        steps.add('F13-06', 'one', True)
        steps.add('F13-04C', 'two', False, 'why')
        steps.add('F13-04B', 'three', False, 'also')
        keep, fixture_checks.keep = fixture_checks.keep, lambda check, text: 'LOG'
        try:
            ok, detail = steps.result('billing-console', 'fine')
        finally:
            fixture_checks.keep = keep
        self.assertFalse(ok)
        self.assertIn('2/3 steps failed', detail)
        self.assertIn('F13-04C two', detail)
        self.assertIn('F13-04B three', detail)

    def test_no_step_is_not_a_pass(self):
        keep, fixture_checks.keep = fixture_checks.keep, lambda check, text: 'LOG'
        try:
            self.assertFalse(b.Steps().result('x', 'y')[0])
        finally:
            fixture_checks.keep = keep

    def test_a_script_without_a_report_is_a_failed_step(self):
        self.assertEqual(b.parse_steps('boom')[0]['ok'], False)
        self.assertEqual(b.parse_steps('noise\n{"steps":[{"step":"a","ok":true}]}\n'), [{'step': 'a', 'ok': True}])


class RelaySeries(unittest.TestCase):
    def test_only_hosted_relay_bytes_count(self):
        paths = [{'mode': 'relay', 'hosting': 'managed', 'upload_bytes': 3, 'download_bytes': 4}, {'mode': 'relay', 'hosting': 'self_hosted', 'upload_bytes': 100, 'download_bytes': 0},
                 {'mode': 'direct', 'hosting': 'managed', 'upload_bytes': 50, 'download_bytes': 0}]
        self.assertEqual(b.hosted_relay_bytes({'totals': {'by_path': paths}}), 7)


class TeamLifecycle(unittest.TestCase):
    def test_the_invoice_count_comes_from_the_invoices_step(self):
        self.assertEqual(b.paid_invoices([{'step': 'a', 'ok': True}, {'step': 'invoices', 'ok': True, 'paid': 2}]), 2)
        self.assertIsNone(b.paid_invoices([{'step': 'invoices completed', 'ok': False}]))
        self.assertIsNone(b.paid_invoices([{'step': 'invoices', 'ok': False, 'paid': 0}]))

    def test_the_removal_must_survive_without_taking_effect_early(self):
        date = '2026-12-27T06:34:08Z'
        kept = {'seats_bought': 3, 'scheduled_seats': 2, 'scheduled_seats_at': date, 'allowance_bytes': 300_000_000_000}
        self.assertTrue(b.removal_kept(kept, date))
        for broken in ({'scheduled_seats': None}, {'seats_bought': 2}, {'scheduled_seats_at': '2027-01-01T00:00:00Z'}, {'allowance_bytes': 200_000_000_000}):
            self.assertFalse(b.removal_kept({**kept, **broken}, date), broken)


class Fence(unittest.TestCase):
    def test_only_a_fresh_throwaway_account_can_be_touched(self):
        sql = workspace_usage.script(USER, 'cap')
        for part in ("'^final-[0-9a-f]{10}@'", "interval '6 hours'", 'count(*) FROM chosen) <> 1', 'consumed_bytes = p.included_bytes', USER):
            self.assertIn(part, sql)
        self.assertNotIn('UPDATE', workspace_usage.script(USER, 'show').replace('FOR UPDATE', ''))

    def test_hostile_or_incomplete_input_is_refused(self):
        for user, action, values in (("x' OR '1'='1", 'cap', ()), ('user_%', 'cap', ()), (USER, 'drop', ()), (USER, 'spend', ()),
                                     (USER, 'spend', ('1; DROP',)), (USER, 'spend', ('-1',)), (USER, 'restore', ('1',)), (USER, 'cap', ('5',))):
            with self.assertRaises(ValueError, msg=(user, action, values)):
                workspace_usage.script(user, action, values)


class RenewalEvidence(unittest.TestCase):
    def test_only_the_charges_count_not_the_zero_trial_invoice(self):
        history = ('3 paid:  Nov 27, 2026 $8.00 Paid Paperboat Pro Nov 27, 2026 $0.00 Paid Trial period for Paperboat Pro '
                   'Nov 27, 2026 $8.00 Paid Paperboat Pro')  # the portal's own text on release A
        self.assertEqual(b.paid_charges(history, '$8.00'), 2)
        self.assertEqual(b.paid_charges('1 paid:  Nov 27, 2026 $8.00 Paid Paperboat Pro', '$8.00'), 1)

    def test_a_moved_grace_is_traced_to_the_retry(self):
        text = b.retry_detail({'attempted_at': 1795815729}, {'attempted_at': 1795815849, 'attempt_count': 1},
                                           '2026-12-04T21:42:09Z', '2026-12-04T21:44:09Z')
        self.assertIn('+120 s from seven days after the first attempt', text)
        self.assertIn('+0 s from seven days after the retry', text)


class Registration(unittest.TestCase):
    def test_every_check_runs_in_smoke_and_is_preflighted(self):
        smoke = (HERE / 'smoke.sh').read_text()
        for check in b.CHECKS:
            self.assertIn('\nfixture_check %s\n' % check, smoke)
            self.assertIn(check, smoke.split('fixture_ids=$(for id in ')[1].split(';')[0].split())
            self.assertIn(check, preflight_fixtures.NEEDS)


@unittest.skipUnless(shutil.which('node'), 'node is not installed')
class BrowserScripts(unittest.TestCase):
    def node(self, source):
        done = subprocess.run(['node', '--input-type=module', '-e', source], cwd=HERE, capture_output=True, text=True, timeout=60)
        self.assertEqual(done.returncode, 0, done.stderr)
        return json.loads(done.stdout)

    def test_only_stripe_test_mode_pages_are_accepted(self):
        got = self.node('''
import { testCheckoutURL } from "./stripe_pay.mjs"
import { testPortalURL } from "./billing_console.mjs"
console.log(JSON.stringify([
  ["https://checkout.stripe.com/g/pay/cs_test_a1B2", "https://checkout.stripe.com/c/pay/cs_test_a1B2#x", "https://checkout.stripe.com/c/pay/cs_live_a1B2",
   "https://evil.example/c/pay/cs_test_a1"].map(testCheckoutURL),
  ["https://billing.stripe.com/p/session?secret=test_YWNj", "https://billing.stripe.com/p/session/subscriptions/sub_1/update?secret=test_YWNj",
   "https://billing.stripe.com/p/session?secret=live_YWNj", "https://evil.example/p/session?secret=test_a",
   "https://billing.stripe.com/p/session?secret=live_YWNj&return_url=https://x.test_a"].map(testPortalURL)]))''')
        self.assertEqual(got, [[True, True, False, False], [True, True, False, False, False]])

    def test_a_page_that_is_not_test_mode_is_never_typed_into(self):
        got = self.node('''
import { payTestCheckout } from "./stripe_pay.mjs"
const page = { url: () => "https://checkout.stripe.com/c/pay/cs_live_abc", getByText: () => { throw new Error("touched") }, frames: () => { throw new Error("touched") } }
console.log(JSON.stringify(await payTestCheckout(page).then(() => "paid", (e) => e.message)))''')
        self.assertIn('refusing to pay', got)

    def test_a_dropped_card_field_is_typed_again_before_submitting(self):
        got = self.node('''
import { keepTyped, sameValue, whyStillThere } from "./stripe_pay.mjs"
const typed = {}, writes = []
let dropped = false
const field = async (name) => ({
  inputValue: async () => typed[name] ?? "",
  fill: async (v) => { typed[name] = v },
  pressSequentially: async (v) => { writes.push(name); typed[name] = name === "Card number" && !dropped ? (dropped = true, "") : v.replace(/(.{4})(?=.)/g, "$1 ") },
})
await keepTyped({ waitForTimeout: async () => {} }, field, [["Card number", "4242424242424242"], ["Security code", "123"]])
const stuck = await keepTyped({ waitForTimeout: async () => {} }, async () => ({ inputValue: async () => "", fill: async () => {}, pressSequentially: async () => {} }), [["Card number", "4242"]]).then(() => "kept", (e) => e.message)
console.log(JSON.stringify([writes.filter((w) => w === "Card number").length, sameValue("12 / 34", "1234"), sameValue("4242 4242", "42424243"), stuck.includes("did not keep")]))''')
        self.assertEqual(got, [2, True, False, True])

    def test_the_scheduled_removal_line_is_exact(self):
        got = self.node('''
import { seatNote, expiredCheckout, sessionOf } from "./billing_console.mjs"
const page = (date, note) => `Plan\\n${date}\\nDec 27, 2026\\nSeats\\n3 seats\\n1 used · 2 free\\n${note}`
const ok = (text, cancelling) => seatNote(text, 3, 2, cancelling)[0]
console.log(JSON.stringify([
  ok(page("Renews", "3 seats until Dec 27, 2026, then 2."), false),
  ok(page("Ends", "3 seats until Dec 27, 2026, then 2 if you keep the plan."), true),
  ok(page("Ends", "3 seats until Dec 27, 2026, then 2."), true),
  ok(page("Renews", "3 seats until Dec 27, 2026, then 2 if you keep the plan."), false),
  ok(page("Renews", "3 seats until Dec 28, 2026, then 2."), false),
  ok(page("Renews", "3 seats until Dec 27, 2026, then 3."), false),
  ok(page("Renews", ""), false),
  [expiredCheckout("This checkout session has expired."), expiredCheckout("Subscribe to Paperboat Pro Pay with card")],
  [sessionOf("https://checkout.stripe.com/c/pay/cs_test_aB1#frag"), sessionOf("https://checkout.stripe.com/") ?? null]]))''')
        self.assertEqual(got, [True, True, False, False, False, False, False, [True, False], ['cs_test_aB1', None]])

    def test_every_paid_invoice_below_the_history_heading_counts(self):
        got = self.node('''
import { historyOf, paidRows, amount } from "./billing_console.mjs"
const text = "Invoice history\\nNov 27\\n$15.00\\nPaid\\nsee invoice history\\nNov 27\\n$30.00\\nPaid"
console.log(JSON.stringify([paidRows(historyOf(text)), paidRows(historyOf("Paid plan, no history")), amount("-$10"), amount("$1,200.50")]))''')
        self.assertEqual(got, [2, 0, -10, 1200.5])

    def test_card_and_plan_text_are_compared_with_billing(self):
        got = self.node('''
import { cardRows, planRows, formatBytes } from "./billing_console.mjs"
const billing = { state: "free", usage_bytes: 5e9, allowance_bytes: 5e9, topup_bytes_purchased: 0, throttled: true, throttle_mbit: 0.512, relay_mbit: 10,
  limits: { machines: { used: 2, limit: 100 } } }
const card = "Slowed\\nto 512 kbit/s until Dec 1\\n5.0 GB of 5.0 GB\\nResets Dec 1\\nRelay\\n1.5 MB\\nEdge\\n0 B\\n5.0 GB a month of traffic included. After that, speed is reduced to 512 kbit/s. Until then relayed connections run at up to 10 Mbit/s"
const plan = "Plan\\nPlan\\nFree\\nTraffic included\\n5.0 GB of 5.0 GB used\\nMachines\\n2 of 100"
const bad = (rows) => rows.filter(([, ok]) => !ok).map(([step]) => step)
console.log(JSON.stringify([formatBytes(1500000), bad(cardRows(card, "true", billing, { relay: 1500000, edge: 0 })), bad(planRows(plan, billing)),
  bad(cardRows(card, "false", billing, { relay: 9e8, edge: 0 })).length]))''')
        self.assertEqual(got, ['1.5 MB', [], [], 2])


if __name__ == '__main__':
    unittest.main()
