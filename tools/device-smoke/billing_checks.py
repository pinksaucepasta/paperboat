#!/usr/bin/env python3
"""The billing and plan rows of fixture_checks.py. Every check makes its own throwaway account(s) (fresh_account.mjs), pays only on Stripe's
TEST-MODE pages with the test card, and deletes the account at the end (which cancels its subscription), on every exit path.

  billing-console           F13-04C: Pro monthly bought on the live console's Checkout reaches the paid overview through the test webhook, after
                            a first Checkout was left unpaid (L38: the next opens, and the abandoned one can no longer be paid).
                            F13-06: the plan screen and the allowance card (relay and edge series, Direct/Relay/Edge cards) per plan, Free and Pro.
                            F13-04B: switching monthly -> yearly -> monthly in Stripe's portal keeps the bandwidth window to the second,
                            the renewal day and time, the allowance and the bytes already spent (seeded by workspace_usage.py).
  bandwidth-throttle-send   F13-03S as the product works today (the plan-limit cut was removed, 02e55fb13: past the allowance traffic is slowed,
                            never cut off): with the fair-use amount used up, `pb send` over a relay path finishes, intact, at the slowed
                            speed, and says why and where to act; the console's card says "Slowed" (F13-06).
  billing-declined-card     F13-04: a declined test card on Checkout leaves the workspace Free and says why; the good card on the same
                            Checkout recovers. (A failed RENEWAL needs a Stripe test clock or key: not here, see below.)
  billing-topup             F13-04T: Free refused, quantity limits, 200 GB bought on the console and credited once; F13-06A: the
                            allowance period; F13-03T: with the plan allowance used up, relayed traffic is taken from the top-up.
  plus-alias-signup         F13-07: plus_alias_signup.mjs.
  billing-team-lifecycle    F13-04, the transitions that need no clock: a team bought on the console's Checkout (upgrade), a seat added mid-cycle
                            (charged at once, one more paid invoice, billing date unchanged) and removed (nothing charged, date unchanged), the
                            plan cancelled and kept in Stripe's portal, the removal surviving it (L37) and shown in the Seats section
                            before, during and after the cancellation (L38). Not here: failed payment -> grace -> suspension -> recovery (needs a
                            Stripe test clock and the test API key, which only the server holds).

Each prints its failed steps in the receipt and keeps every step in the results directory. A step that fails never stops the others.
"""
import datetime
import json
import os
from pathlib import Path
import shlex
import subprocess
import tempfile
import time

import fixture_checks as fx
import workspace_usage

SPENT = 7_000_000
SEND_BYTES = 1_500_000
FREE_SLOW_BYTES_PER_SECOND = 64_000  # config FairUse.EdgeFreeBytesPerSecond: 512 kbit/s
MAX_LOAD = 12


class Steps:
    """[cell] step ok detail, collected: the receipt names every failed one."""

    def __init__(self):
        self.rows = []

    def add(self, cell, step, ok, detail=''):
        self.rows.append((cell, step, bool(ok), str(detail)[:300]))
        print('%s %-7s %s | %s' % ('ok ' if ok else 'BAD', cell, step, str(detail)[:200]), flush=True)
        return bool(ok)

    def result(self, check, summary):
        text = '\n'.join('%s %s %s | %s' % ('PASS' if ok else 'FAIL', cell, step, detail) for cell, step, ok, detail in self.rows)
        failed = ['%s %s (%s)' % (cell, step, detail[:80]) for cell, step, ok, detail in self.rows if not ok]
        log = fx.keep(check, text)  # every step with its numbers, passing runs too: the evidence for the checklist cells
        if self.rows and not failed:
            return True, '%s; %d/%d steps, log %s' % (summary, len(self.rows), len(self.rows), log)
        return False, '%d/%d steps failed: %s log %s' % (len(failed), len(self.rows), '; '.join(failed)[:260] or 'no step ran', log)


def parse_steps(output):
    """The {"steps": [...]} line a billing browser script ends with; one failed step when there is none."""
    record = fx.last_json(output)
    return record.get('steps') or [{'step': 'the browser script reported its steps', 'ok': False, 'detail': ' '.join(output.split())[-250:]}]


def calm(limit=MAX_LOAD):
    """The controller is shared: wait before another headless browser while its load is high."""
    deadline = time.monotonic() + 900
    while os.getloadavg()[0] >= limit and time.monotonic() < deadline:
        time.sleep(20)


def browser(m, steps, cell, state, script, *args, timeout=900, team=None):
    """Runs one browser script and records its steps under `cell`; returns them (all recorded, a failed one stops none)."""
    calm()
    try:
        env = dict(m.env, BILLING_TEAM=team or '', **({'STATE': state} if state else {}))
        done = fx.run(['node', str(fx.HERE / script), *args], env=env, check=False, timeout=timeout)
        found = parse_steps(done.stdout + done.stderr)
    except subprocess.TimeoutExpired:
        found = [{'step': 'the browser script finished', 'ok': False, 'detail': 'no result in %d s' % timeout}]
    label = ' '.join(a for a in args if not a.startswith('/'))
    for s in found:
        steps.add(cell, '%s: %s' % (label, s['step']) if label else s['step'], s['ok'], s.get('detail', ''))
    return found


def paid_invoices(found):
    """The number of paid invoices the `invoices` command saw, or None when that step did not pass."""
    return next((s['paid'] for s in found if 'paid' in s and s.get('ok')), None)


def api(m, state, path):
    calm()
    reply = json.loads(m.node('console_api.mjs', 'GET', path, state=state, timeout=180).stdout.strip().splitlines()[-1])
    if reply['status'] != 200:
        raise RuntimeError('GET %s answered %s' % (path, reply['status']))
    return reply['body'].get('data', reply['body'])


def when(value):
    return datetime.datetime.fromisoformat(value.replace('Z', '+00:00'))


def anchor_rows(first, now, interval, spent):
    """F13-04B: the billing overview after a switch to `interval`, against the first paid (monthly) one: [(step, ok, detail)]."""
    product = (now.get('product') or {}).get('interval')
    bought, renews = when(first['period_start']), when(now['period_end'])
    next_year = bought.replace(year=bought.year + 1, day=28 if (bought.month, bought.day) == (2, 29) else bought.day)
    want = next_year if interval == 'year' else when(first['period_end'])
    return [
        ('the plan is billed by the %s' % interval, product == interval, 'interval %s' % product),
        ('the bandwidth window keeps the paid billing day and time', (now['allowance_period_start'], now['allowance_period_end']) == (first['allowance_period_start'], first['allowance_period_end']),
         '%s..%s, was %s..%s' % (now['allowance_period_start'], now['allowance_period_end'], first['allowance_period_start'], first['allowance_period_end'])),
        ('the renewal keeps the billing day and time', renews == want, 'renews %s, want %s' % (now['period_end'], want.isoformat())),
        ('the monthly allowance is unchanged', now['allowance_bytes'] == first['allowance_bytes'], '%s' % now['allowance_bytes']),
        ('the bytes already spent are kept', now['usage_bytes'] >= spent, 'usage %s, spent %s' % (now['usage_bytes'], spent)),
    ]


def billing_console(m):
    steps = Steps()
    _, state = m.account('bill')
    user = api(m, state, '/v1/me')['id']
    browser(m, steps, 'F13-06', state, 'billing_console.mjs', 'card', 'free')
    browser(m, steps, 'F13-04C', state, 'billing_console.mjs', 'checkout')
    paid = api(m, state, '/v1/billing/workspaces/personal')
    if paid.get('product_code') != 'pro-month':
        steps.add('F13-04B', 'a paid monthly plan to switch', False, 'not run: Checkout left product=%s' % paid.get('product_code'))
        return steps.result('billing-console', '')
    browser(m, steps, 'F13-06', state, 'billing_console.mjs', 'card', 'pro')
    seeded = workspace_usage.run(user, 'spend', str(SPENT))
    first = api(m, state, '/v1/billing/workspaces/personal')
    steps.add('F13-04B', 'the seeded spent bytes show on the monthly plan', first['usage_bytes'] == SPENT and seeded['observed'] == SPENT, 'usage %s' % first['usage_bytes'])
    for interval in ('year', 'month'):
        browser(m, steps, 'F13-04B', state, 'billing_console.mjs', 'portal', interval)
        now = api(m, state, '/v1/billing/workspaces/personal')
        for step, ok, detail in anchor_rows(first, now, interval, SPENT):
            steps.add('F13-04B', 'after the %sly switch: %s' % (interval, step), ok, detail)
    return steps.result('billing-console', 'Free card, console Checkout to Pro monthly, Pro card, yearly and monthly switch keep the bandwidth window and spend')


def send_events(stderr):
    """The JSON events `pb send --json` writes to stderr."""
    events = []
    for line in stderr.splitlines():
        try:
            value = json.loads(line)
        except ValueError:
            continue
        if isinstance(value, dict) and value.get('event'):
            events.append(value)
    return events


def throttle_rows(code, stdout, stderr, seconds, size, as_json):
    """F13-03S today: the capped send's result [(step, ok, detail)]."""
    said = ' '.join((stdout + ' ' + stderr).split())
    rows = [('pb send is not cut: it exits 0', code == 0, 'exit %s %s' % (code, said[-200:])),
            ('no cancellation or quota refusal is shown', 'Operation canceled' not in said and 'bandwidth_quota_exhausted' not in said, said[-200:]),
            # the slow speed is 64 kB/s; a first burst makes it a little quicker, the unthrottled Free relay speed is twenty times that
            ('it runs at the slowed speed', seconds >= size / (FREE_SLOW_BYTES_PER_SECOND * 2), '%d bytes in %.1f s = %d B/s (slow speed %d B/s)' % (size, seconds, size / max(seconds, 0.001), FREE_SLOW_BYTES_PER_SECOND))]
    if as_json:
        notice = [e for e in send_events(stderr) if e['event'] == 'transfer_throttled']
        ok = bool(notice) and notice[0].get('reason') == 'allowance_used' and notice[0].get('plan_mbit_per_second') == 0.512 and 'Billing' in notice[0].get('recovery', '')
        rows.append(('--json reports transfer_throttled with the speed and the way out', ok, json.dumps(notice[:1])[:250]))
        reply = fx.last_json(stdout)
        rows.append(('--json stdout stays one successful receipt', reply.get('ok') is True and len(stdout.strip().splitlines()) == 1, stdout.strip()[:150]))
    else:
        rows.append(('it names the used-up allowance, the slowed speed and Billing', all(part in said for part in ('used its traffic allowance', 'throttled', '0.512 Mbit/s', 'in Billing')), said[-300:]))
    return rows


def machine_send(m, prefix, peer, filename, as_json):
    """(exit code, stdout, stderr, seconds) of one `pb send` on the machine."""
    command = 'cd ~ && %s send %s --to %s%s' % (fx.PB, filename, peer, ' --json' if as_json else '')
    began = time.monotonic()
    done = fx.run(shlex.split(prefix) + ['bash -lc ' + shlex.quote('timeout 600 sh -c ' + shlex.quote(command))], check=False, timeout=660)
    return done.returncode, done.stdout, done.stderr, time.monotonic() - began


def make_file(m, prefix, filename, size):
    return m.at(prefix, 'head -c %d /dev/urandom > %s && sha256sum %s | cut -d" " -f1' % (size, filename, filename)).stdout.strip()


def received(m, prefix, filename):
    return m.at(prefix, "f=$(find ~ -maxdepth 4 -name %s | head -1); [ -n \"$f\" ] && sha256sum \"$f\" | cut -d' ' -f1" % shlex.quote(filename), check=False).stdout.strip()


def relay_ping(m, prefix, peer, seconds=180):
    """(True, line) once `pb ping` reaches the peer over a relay."""
    deadline, out = time.monotonic() + seconds, ''
    while time.monotonic() < deadline:
        done = m.at(prefix, '%s ping %s --count 3' % (fx.PB, peer), check=False, timeout=90)
        out = ' '.join((done.stdout + done.stderr).split())
        if not done.returncode and 'path=regional_relay' in out and 'path=direct' not in out:
            return True, out[-120:]
        time.sleep(8)
    return False, out[-250:]


# outbound UDP is dropped except DNS, as in the hand proof: no direct path is left, the relay is reached over TCP
BLOCK_UDP = 'table inet pbsmoke_relayonly { chain out { type filter hook output priority 0; udp dport 53 accept; meta l4proto udp drop; }; }'


def bandwidth_throttle_send(m):
    steps = Steps()
    _, state = m.account('cap')
    sender, _, prefix = m.machine('snd', state=state)
    peer, _, peer_prefix = m.machine('rcv', state=state)
    fx.ready(prefix, peer)
    fx.remote('echo %s | %s exec -i -u root %s nft -f -' % (shlex.quote(BLOCK_UDP), fx.DOCKER, sender))
    ok, detail = relay_ping(m, prefix, peer)
    if not steps.add('F13-03S', 'with UDP blocked the sender reaches the peer over a relay', ok, detail):
        return steps.result('bandwidth-throttle-send', '')
    digest = make_file(m, prefix, 'base.bin', SEND_BYTES)
    code, out, err, seconds = machine_send(m, prefix, peer, 'base.bin', False)
    steps.add('F13-03S', 'within the allowance the same send is fast and says nothing about it',
              code == 0 and received(m, peer_prefix, 'base.bin') == digest and 'throttled' not in out + err and seconds < SEND_BYTES / (FREE_SLOW_BYTES_PER_SECOND * 2),
              'exit %s in %.1f s %s' % (code, seconds, ' '.join(err.split())[-150:]))
    user = api(m, state, '/v1/me')['id']
    api(m, state, '/v1/billing/workspaces/personal')  # the overview creates the current usage period
    capped = workspace_usage.run(user, 'cap')
    m.callback(workspace_usage.run, user, 'restore', *map(str, capped['before']))
    overview = api(m, state, '/v1/billing/workspaces/personal')
    steps.add('F13-03S', 'billing reports the capped workspace as throttled at 512 kbit/s', overview['throttled'] and overview['throttle_mbit'] == 0.512,
              'throttled=%s mbit=%s' % (overview['throttled'], overview['throttle_mbit']))
    # the relay learns the account's new speed with its next lease: the first capped send may still run fast, then it must be slow
    began, attempt = time.monotonic(), 0
    time.sleep(45)
    while True:
        attempt += 1
        name = 'capped%d.bin' % attempt
        digest = make_file(m, prefix, name, SEND_BYTES)
        code, out, err, seconds = machine_send(m, prefix, peer, name, False)
        rows = throttle_rows(code, out, err, seconds, SEND_BYTES, False)
        if rows[2][1] or code or time.monotonic() - began > 300:
            break
        time.sleep(30)
    for step, ok, detail in rows:
        steps.add('F13-03S', step, ok, detail)
    steps.add('F13-03S', 'the slowed file arrives intact', received(m, peer_prefix, name) == digest, name)
    digest = make_file(m, prefix, 'capped-json.bin', SEND_BYTES)
    for step, ok, detail in throttle_rows(*machine_send(m, prefix, peer, 'capped-json.bin', True), SEND_BYTES, True):
        steps.add('F13-03S', '--json: ' + step, ok, detail)
    ok, detail = relay_ping(m, prefix, peer, 60)
    steps.add('F13-03S', 'the path is still the relay', ok, detail)
    # a machine reports its relayed bytes at most every five minutes (cli/internal/bandwidth batchInterval): wait for them, so the
    # card's relay series is compared with real traffic
    relayed = 0
    deadline = time.monotonic() + 480
    while True:
        relayed = hosted_relay_bytes(api(m, state, usage_path(overview, datetime.datetime.now(datetime.timezone.utc))))
        # at least three sends: the one before the cap and two after it, so the capped traffic itself is in the series
        if relayed >= 3 * SEND_BYTES or time.monotonic() > deadline:
            break
        time.sleep(60)
    steps.add('F13-06', 'the relayed sends, the capped ones too, are counted in the relay series', relayed >= 3 * SEND_BYTES,
              '%d relayed bytes counted for at least three sends of %d' % (relayed, SEND_BYTES))
    browser(m, steps, 'F13-06', state, 'billing_console.mjs', 'card', 'throttled')
    return steps.result('bandwidth-throttle-send', 'capped workspace: relayed pb send finishes intact at 512 kbit/s and says why; the console card says Slowed')


def usage_path(billing, now):
    """The usage query of the allowance window, as the console asks it."""
    return '/v1/usage?start=%s&end=%s&limit=200&workspace=personal' % (billing['allowance_period_start'], now.strftime('%Y-%m-%dT%H:%M:%SZ'))


def hosted_relay_bytes(usage):
    return sum(p['upload_bytes'] + p['download_bytes'] for p in usage['totals']['by_path'] if p['mode'] == 'relay' and p['hosting'] == 'managed')


def plus_alias_signup(m):
    steps = Steps()
    _, state = m.account('alias')
    work = Path(tempfile.gettempdir()) / ('fx-solo-' + m.run_id)
    # registered before the sign-ups run: whatever account exists in these directories at the end is deleted (the later one first: LIFO)
    m.callback(delete_if_made, m, work)
    m.callback(delete_if_made, m, Path(str(work) + '-later'))
    browser(m, steps, 'F13-07', None, 'plus_alias_signup.mjs', str(Path(state).parent), str(work), timeout=1500)
    return steps.result('plus-alias-signup', 'first sign-up wins: plus addresses of an existing account are refused naming it; a plus sign-up with no base account works and a later base sign-up is refused naming the plus address')


def delete_if_made(m, directory):
    if (directory / 'state.json').is_file():
        m._delete_account(str(directory))
    elif (directory / 'account.json').is_file():
        m.leftovers.append('a sign-up that never reached the console may remain at the identity provider; its disposable address is in %s/account.json' % directory)


def removal_kept(billing, billing_date):
    """Three seats stay until the billing date, and two are what renews then."""
    return tuple(billing.get(k) for k in ('seats_bought', 'scheduled_seats', 'scheduled_seats_at', 'allowance_bytes')) == (3, 2, billing_date, 300_000_000_000)


def billing_team_lifecycle(m):
    steps = Steps()
    _, state = m.account('team')
    team = 'pbbill-' + m.run_id
    browser(m, steps, 'F13-04', state, 'billing_console.mjs', 'team-checkout', team, '2')
    path = '/v1/billing/workspaces/teams/' + team
    try:
        bought = api(m, state, path)
    except RuntimeError as error:
        bought = {'state': str(error)}
    if bought.get('state') != 'active' or not bought.get('allowance_bytes'):  # an unpaid team is never changed
        steps.add('F13-04', 'a paid team to change', False, 'not run: Checkout left state=%s' % bought.get('state'))
        return steps.result('billing-team-lifecycle', '')
    on_team = {'team': team}
    invoices = paid_invoices(browser(m, steps, 'F13-04', state, 'billing_console.mjs', 'invoices', **on_team))
    browser(m, steps, 'F13-04', state, 'billing_console.mjs', 'seats', '3', **on_team)
    grown = api(m, state, path)
    steps.add('F13-04', 'three seats pool three seats of allowance', grown['allowance_bytes'] == 3 * bought['allowance_bytes'] // 2,
              '%s -> %s' % (bought['allowance_bytes'], grown['allowance_bytes']))
    later = paid_invoices(browser(m, steps, 'F13-04', state, 'billing_console.mjs', 'invoices', **on_team))
    steps.add('F13-04', 'the added seat was charged at once: one more paid invoice', None not in (invoices, later) and later == invoices + 1, '%s -> %s paid invoices' % (invoices, later))
    browser(m, steps, 'F13-04', state, 'billing_console.mjs', 'seats', '2', **on_team)
    browser(m, steps, 'F13-04', state, 'billing_console.mjs', 'portal', 'cancel', **on_team)
    cancelled = api(m, state, path)
    steps.add('F13-04', 'cancelling ends the plan at the billing date, not now', cancelled['cancel_at_period_end'] and cancelled['state'] == 'active' and cancelled['period_end'] == bought['period_end'],
              'state=%s cancel_at_period_end=%s ends %s' % (cancelled['state'], cancelled['cancel_at_period_end'], cancelled['period_end']))
    browser(m, steps, 'F13-04', state, 'billing_console.mjs', 'seat-note', '3', '2', 'cancelling', **on_team)
    browser(m, steps, 'F13-04', state, 'billing_console.mjs', 'portal', 'keep', **on_team)
    browser(m, steps, 'F13-04', state, 'billing_console.mjs', 'seat-note', '3', '2', 'renewing', **on_team)
    kept = api(m, state, path)
    steps.add('F13-04', 'keeping the plan renews it on the same date', not kept['cancel_at_period_end'] and kept['state'] == 'active' and kept['period_end'] == bought['period_end'],
              'state=%s cancel_at_period_end=%s renews %s' % (kept['state'], kept['cancel_at_period_end'], kept['period_end']))
    # the seat removed above was never given back by the owner: undoing the cancellation must not undo the removal
    steps.add('F13-04', 'L37: keeping the plan keeps the scheduled seat removal: 2 seats from the billing date', removal_kept(kept, bought['period_end']),
              'scheduled_seats=%s at %s, seats now %s' % (kept.get('scheduled_seats'), kept.get('scheduled_seats_at'), kept.get('seats_bought')))
    return steps.result('billing-team-lifecycle', 'team bought on console Checkout, seat added (invoiced at once) and removed on a fixed billing date, cancelled and kept')


TERMS_VERSION = '2026-11-19'  # server/internal/legalconsent: the wording a purchase request accepts


def api_call(m, state, method, path, body=None):
    """(status, body) of one console API call, a refusal included."""
    calm()
    arguments = ('console_api.mjs', method, path) + ((json.dumps(body),) if body is not None else ())
    reply = json.loads(m.node(*arguments, state=state, timeout=180).stdout.strip().splitlines()[-1])
    return reply['status'], reply['body']


def billing_declined_card(m):
    steps = Steps()
    _, state = m.account('decl')
    browser(m, steps, 'F13-04', state, 'billing_console.mjs', 'declined')
    now = api(m, state, '/v1/billing/workspaces/personal')
    steps.add('F13-04', 'the API shows one active Pro subscription after the recovery', now.get('state') == 'active' and now.get('product_code') == 'pro-month' and not now.get('renewal_grace_until'),
              'state=%s product=%s grace=%s' % (now.get('state'), now.get('product_code'), now.get('renewal_grace_until')))
    return steps.result('billing-declined-card', 'a declined card leaves the workspace Free with the reason shown; the good card on the same Checkout recovers')


def topup_request(product, quantity, origin):
    return {'product_code': product.get('code', ''), 'product_version': product.get('version', 0), 'quantity': quantity,
            'success_url': origin + '/billing?checkout=done', 'cancel_url': origin + '/billing', 'terms_version': TERMS_VERSION}


def spend_rows(before, after, sent):
    """F13-03T after a relayed send on a workspace whose plan allowance is used up and which owns a top-up: [(step, ok, detail)].
    A machine reports relayed bytes as upload and download hops, so a send is debited at least once its size."""
    spent = before['topup_bytes_remaining'] - after['topup_bytes_remaining']
    return [
        ('the relayed bytes are taken from the top-up', sent <= spent <= 4 * sent + 2_000_000, 'top-up %s -> %s (-%d) for %d bytes sent' % (before['topup_bytes_remaining'], after['topup_bytes_remaining'], spent, sent)),
        ('the bought amount stays on record', after['topup_bytes_purchased'] == before['topup_bytes_purchased'], '%s' % after['topup_bytes_purchased']),
        ('with top-up left the workspace is not slowed', after['throttled'] is False, 'throttled=%s' % after['throttled']),
    ]


def billing_topup(m):
    """F13-04T buy, F13-06A allowance period, F13-03T the plan is spent before the top-up."""
    steps = Steps()
    _, state = m.account('topup')
    origin = m.env['ORIGIN']
    free = api(m, state, '/v1/billing/workspaces/personal')
    start, end = free.get('allowance_period_start'), free.get('allowance_period_end')
    steps.add('F13-06A', 'a Free overview states its monthly allowance period around now',
              bool(start and end) and when(start) <= datetime.datetime.now(datetime.timezone.utc) < when(end) and 27 <= (when(end) - when(start)).days <= 31, '%s..%s' % (start, end))
    products = api(m, state, '/v1/billing/workspaces/personal/products')
    product = next((p for p in products.get('items', []) if p.get('catalog_type') == 'top_up' and p.get('price_verified')), {})
    steps.add('F13-04T', 'the catalog serves one verified top-up product of 100 GB units', product.get('unit_bytes') == 10**11 and product.get('max_quantity', 0) >= 1, json.dumps(product)[:200])
    status, body = api_call(m, state, 'POST', '/v1/billing/workspaces/personal/topups', topup_request(product, 1, origin))
    steps.add('F13-04T', 'a Free workspace cannot buy a top-up (409 paid_plan_required)', status == 409 and 'paid_plan_required' in json.dumps(body), '%s %s' % (status, json.dumps(body)[:200]))
    browser(m, steps, 'F13-04C', state, 'billing_console.mjs', 'checkout')
    paid = api(m, state, '/v1/billing/workspaces/personal')
    if paid.get('product_code') != 'pro-month':
        steps.add('F13-04T', 'a paid workspace to top up', False, 'not run: Checkout left product=%s' % paid.get('product_code'))
        return steps.result('billing-topup', '')
    steps.add('F13-06A', 'the paid overview states a monthly allowance period inside the paid period',
              when(paid['allowance_period_start']) <= datetime.datetime.now(datetime.timezone.utc) < when(paid['allowance_period_end']) <= when(paid['period_end']),
              '%s..%s, paid to %s' % (paid['allowance_period_start'], paid['allowance_period_end'], paid['period_end']))
    for quantity in (0, product.get('max_quantity', 100) + 1):
        status, body = api_call(m, state, 'POST', '/v1/billing/workspaces/personal/topups', topup_request(product, quantity, origin))
        steps.add('F13-04T', 'quantity %d is refused' % quantity, status == 400, '%s %s' % (status, json.dumps(body)[:200]))
    browser(m, steps, 'F13-04T', state, 'billing_console.mjs', 'topup', '2')
    sender, _, prefix = m.machine('tsnd', state=state)
    peer, _, peer_prefix = m.machine('trcv', state=state)
    fx.ready(prefix, peer)
    fx.remote('echo %s | %s exec -i -u root %s nft -f -' % (shlex.quote(BLOCK_UDP), fx.DOCKER, sender))
    ok, detail = relay_ping(m, prefix, peer)
    if not steps.add('F13-03T', 'with UDP blocked the sender reaches the peer over a relay', ok, detail):
        return steps.result('billing-topup', '')
    user = api(m, state, '/v1/me')['id']
    capped = workspace_usage.run(user, 'cap')
    m.callback(workspace_usage.run, user, 'restore', *map(str, capped['before']))
    before = api(m, state, '/v1/billing/workspaces/personal')
    steps.add('F13-03T', 'plan allowance used up with top-up left: not slowed', before['throttled'] is False and before['topup_bytes_remaining'] == 2 * 10**11,
              'throttled=%s remaining=%s' % (before['throttled'], before.get('topup_bytes_remaining')))
    time.sleep(45)  # the relay learns the account's allowance with its next lease
    digest = make_file(m, prefix, 'topup.bin', SEND_BYTES)
    code, out, err, seconds = machine_send(m, prefix, peer, 'topup.bin', False)
    steps.add('F13-03T', 'the send over the relay runs at full speed and says nothing about a limit',
              code == 0 and received(m, peer_prefix, 'topup.bin') == digest and 'throttled' not in out + err and seconds < SEND_BYTES / (FREE_SLOW_BYTES_PER_SECOND * 2),
              'exit %s in %.1f s %s' % (code, seconds, ' '.join(err.split())[-150:]))
    # a machine reports its relayed bytes at most every five minutes (cli/internal/bandwidth batchInterval)
    deadline, after = time.monotonic() + 480, before
    while time.monotonic() < deadline:
        after = api(m, state, '/v1/billing/workspaces/personal')
        if before['topup_bytes_remaining'] - after['topup_bytes_remaining'] >= SEND_BYTES:
            break
        time.sleep(45)
    for step, ok, detail in spend_rows(before, after, SEND_BYTES):
        steps.add('F13-03T', step, ok, detail)
    shown = workspace_usage.run(user, 'show')
    steps.add('F13-03T', 'the used-up plan counter does not go past its included amount', shown['consumed'] == shown['included'], 'consumed %s of %s' % (shown['consumed'], shown['included']))
    return steps.result('billing-topup', 'Free refused, 200 GB bought once on Checkout, quantity limits, allowance period, and relayed traffic past the plan is taken from the top-up')


CHECKS = {'billing-declined-card': billing_declined_card, 'billing-topup': billing_topup, 'billing-team-lifecycle': billing_team_lifecycle, 'billing-console': billing_console, 'bandwidth-throttle-send': bandwidth_throttle_send, 'plus-alias-signup': plus_alias_signup}
