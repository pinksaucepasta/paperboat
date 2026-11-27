#!/usr/bin/env python3
"""The billing and plan rows of fixture_checks.py. Every check makes its own throwaway account(s) (fresh_account.mjs), pays only on Stripe's
TEST-MODE pages with the test card, and deletes the account at the end (which cancels its subscription), on every exit path.

  billing-console           F13-04C: Pro monthly bought on the live console's Checkout reaches the paid overview through the test webhook.
                            F13-06: the plan screen and the allowance card (relay and edge series, Direct/Relay/Edge cards) per plan, Free and Pro.
                            F13-04B: switching monthly -> yearly -> monthly in Stripe's portal keeps the bandwidth window to the second,
                            the renewal day and time, the allowance and the bytes already spent (seeded by workspace_usage.py).
  bandwidth-throttle-send   F13-03S as the product works today (the plan-limit cut was removed, 02e55fb13: past the allowance traffic is slowed,
                            never cut off): with the fair-use amount used up, `pb send` over a relay path finishes, intact, at the slowed
                            speed, and says why and where to act; the console's card says "Slowed" (F13-06).
  plus-alias-signup         F13-07: plus_alias_signup.mjs.

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


def browser(m, steps, cell, state, script, *args, timeout=900):
    calm()
    try:
        done = m.node(script, *args, state=state, check=False, timeout=timeout)
        found = parse_steps(done.stdout + done.stderr)
    except subprocess.TimeoutExpired:
        found = [{'step': 'the browser script finished', 'ok': False, 'detail': 'no result in %d s' % timeout}]
    label = ' '.join(a for a in args if not a.startswith('/'))
    # a list, not a generator: every step is recorded even after a failed one
    return all([steps.add(cell, '%s: %s' % (label, s['step']) if label else s['step'], s['ok'], s.get('detail', '')) for s in found])


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
    time.sleep(90)  # the relayed bytes reach the usage ledger with the machines' usage reports
    browser(m, steps, 'F13-06', state, 'billing_console.mjs', 'card', 'throttled')
    return steps.result('bandwidth-throttle-send', 'capped workspace: relayed pb send finishes intact at 512 kbit/s and says why; the console card says Slowed')


def plus_alias_signup(m):
    steps = Steps()
    _, state = m.account('alias')
    work = Path(tempfile.gettempdir()) / ('fx-solo-' + m.run_id)
    # registered before the sign-ups run: whatever account exists in these directories at the end is deleted (the later one first: LIFO)
    m.callback(delete_if_made, m, work)
    m.callback(delete_if_made, m, Path(str(work) + '-later'))
    browser(m, steps, 'F13-07', None, 'plus_alias_signup.mjs', str(Path(state).parent), str(work), timeout=1500)
    return steps.result('plus-alias-signup', 'plus addresses of an existing account are refused by name; one with no base account signs up and is not joined by a later base sign-up')


def delete_if_made(m, directory):
    if (directory / 'state.json').is_file():
        m._delete_account(str(directory))
    elif (directory / 'account.json').is_file():
        m.leftovers.append('a sign-up that never reached the console may remain at the identity provider; its disposable address is in %s/account.json' % directory)


CHECKS = {'billing-console': billing_console, 'bandwidth-throttle-send': bandwidth_throttle_send, 'plus-alias-signup': plus_alias_signup}
