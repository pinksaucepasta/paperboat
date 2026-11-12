#!/usr/bin/env python3
"""C280 / C281: `pb tunnel ip-rules` and `pb preview ip-rules` against the edge, from two real source addresses.

A disposable enrolled machine (reached through --machine-cmd, a command prefix such as
`ssh pbvm4 sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 small-t1`) runs origin_proxy_matrix/app.py
(it answers with the X-Forwarded-For the edge sent) and a tunnel or a preview in front of it. Two observers fetch the public URL:
A runs on this host, B through --observer-b-cmd (e.g. `ssh ubuntu@anvit`), so the edge sees two different client addresses.
Checks: show (empty), deny one address, allow-only, each flag replaces only its own list, deny beats allow, `--allow` /0 and a
malformed address are refused and change nothing, a CIDR form, `--clear`. Everything it creates it removes again.

Usage: ip_rules_rows.py --kind tunnel|preview --machine-cmd '<prefix>' --observer-b-cmd '<prefix>' [--pb /home/pbt/.local/bin/pb]
"""
import argparse
import ipaddress
import json
import shlex
import sys
import time
from pathlib import Path

from machine_ctl import Failure, Machine, remote, run
# An owner's deny must hold within seconds: the edge refreshes its ingress snapshot every 5 s (IngressRefreshInterval) and the
# server publishes a change at once, so the check allows 15 s from the PUT to the refusal, the same for tunnels and previews.
RULE_BOUND = 15
ORIGIN_PORT = '9317'  # the machine's daemon owns 127.100.0.x ports for local access, so the origin binds loopback on this port


def find_url(value):
    if isinstance(value, str):
        return value if value.startswith('https://') else None
    for child in (value.values() if isinstance(value, dict) else value if isinstance(value, list) else []):
        found = find_url(child)
        if found:
            return found
    return None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--kind', choices=['tunnel', 'preview'], required=True)
    parser.add_argument('--machine-cmd', default='', help='command prefix that runs a command on the machine (not with --windows)')
    parser.add_argument('--windows', action='store_true', help='the machine is the Windows device selected by SMOKE_WINDOWS_SSH and PB_SMOKE_WINDOWS_PB')
    parser.add_argument('--observer-b-cmd', required=True)
    parser.add_argument('--pb', default='/home/pbt/.local/bin/pb', help='pb on the machine (ignored with --windows)')
    parser.add_argument('--output', default='')
    parser.add_argument('--wait', type=int, default=RULE_BOUND, help='seconds a rule may take to reach the edge (the bound of the check)')
    args = parser.parse_args()
    try:
        machine = Machine(args.machine_cmd, args.pb, args.windows)
    except Failure as error:
        parser.error(str(error))
    observer_b = shlex.split(args.observer_b_cmd)
    row = 'C280' if args.kind == 'tunnel' else 'C281'
    name = 'laners-ip-%d' % int(time.time())
    steps, cleanup, latencies = [], [], []

    pb = machine.pb

    def good(*arguments, timeout=180):
        code, value = pb(*arguments, '--json', timeout=timeout)
        if code != 0:
            raise Failure('%s: %s' % (' '.join(arguments), str(value)[:300]))
        return value

    def refused(*arguments):
        code, value = pb(*arguments, '--json')
        if code == 0:
            raise Failure('%s succeeded but must be refused' % ' '.join(arguments))
        return value

    def fetch(observer, url):
        code, out, err = run(remote(observer, ['curl', '-sS', '-m', '25', '-o', '-', '-w', '\\n%{http_code}', url]), 60)
        body, _, status = out.rpartition('\n')
        return (int(status) if status.strip().isdigit() else 0), body

    def eventually(observer, url, want, label, seconds=None):
        deadline, last = time.time() + (seconds or args.wait), None
        started = time.time()
        while time.time() < deadline:
            last = fetch(observer, url)
            if last[0] == want:
                latencies.append(round(time.time() - started, 1))
                return last
            time.sleep(1)
        raise Failure('%s: wanted HTTP %d, last %s' % (label, want, last and last[0]))

    def step(label, function):
        started = time.time()
        try:
            function()
            steps.append({'step': label, 'status': 'PASS', 'seconds': round(time.time() - started, 1)})
        except Exception as error:  # noqa: BLE001 - keep going so the cleanup runs and the report shows every failure
            steps.append({'step': label, 'status': 'FAIL', 'seconds': round(time.time() - started, 1), 'detail': str(error)[:400]})

    resource = {}
    try:
        cleanup.append(machine.start_origin(ORIGIN_PORT, 'ip-rules-app'))
        if args.kind == 'tunnel':
            created = good('tunnel', 'create', name, '--port', ORIGIN_PORT, '--duration', '30m', '--wait', '--timeout', '90s', timeout=200)
            resource['id'], resource['url'] = created['tunnel']['id'], created['tunnel']['stable_endpoint']

            def delete():
                code, value = pb('tunnel', 'delete', resource['id'], '--json')
                token = (value.get('confirmation_token') if isinstance(value, dict) else None)
                if token:
                    pb('tunnel', 'delete', resource['id'], '--confirm', token, '--json')
            cleanup.append(delete)
        else:
            created = good('preview', ORIGIN_PORT, '--background', '--ttl', '30m', timeout=200)
            resource['id'] = created.get('id') or (created.get('preview') or {}).get('id')
            resource['url'] = find_url(created)
            if not resource['id'] or not resource['url']:
                raise Failure('preview receipt lacks an id or an https URL: ' + json.dumps(created)[:200])

            def stop():
                pb('preview', 'stop', resource['id'], '--json')
                pb('preview', 'delete', resource['id'], '--json')
            cleanup.append(stop)
        group = [args.kind, 'ip-rules', resource['id']]
        url = resource['url']
        addresses = {}

        def net(address):  # the server stores a single address as its /32 or /128
            return str(ipaddress.ip_network(address, strict=False))

        def lists():
            value = good(*group)
            return sorted(net(x) for x in value.get('allow') or []), sorted(net(x) for x in value.get('deny') or [])

        def baseline():
            for who, observer in (('a', []), ('b', observer_b)):
                status, body = eventually(observer, url, 200, 'baseline ' + who, 180)
                addresses[who] = json.loads(body)['xff'].split(',')[0].strip()
            if addresses['a'] == addresses['b']:
                raise Failure('both observers reach the edge from %s: pick another --observer-b-cmd host' % addresses['a'])
            if lists() != ([], []):
                raise Failure('a new %s starts with ip rules: %s' % (args.kind, lists()))

        def deny_one():
            good(*group, '--deny', addresses['a'])
            if lists() != ([], [net(addresses['a'])]):
                raise Failure('lists after --deny: %s' % (lists(),))
            eventually([], url, 403, 'denied address A')
            eventually(observer_b, url, 200, 'other address B stays allowed')
            # An account's edges refresh their snapshots on their own 5 s ticks, so a second request may still reach an edge that has not
            # applied the rule; the refusal page is held to the same bound as every other answer.
            deadline = time.time() + args.wait
            while True:
                status, body = fetch([], url)
                if status == 403 and 'restricted' in body:
                    break
                if time.time() > deadline:
                    raise Failure('the refusal page does not say the link is restricted (last HTTP %d)' % status)
                time.sleep(1)

        def allow_only_replaces_one_list():
            good(*group, '--allow', addresses['b'])
            if lists() != ([net(addresses['b'])], [net(addresses['a'])]):
                raise Failure('--allow replaced more than the allow list: %s' % (lists(),))
            eventually([], url, 403, 'A is denied')
            eventually(observer_b, url, 200, 'B is allowed')

        def deny_beats_allow():
            good(*group, '--deny', addresses['b'])
            if lists() != ([net(addresses['b'])], [net(addresses['b'])]):
                raise Failure('--deny replaced more than the deny list: %s' % (lists(),))
            eventually(observer_b, url, 403, 'B is both allowed and denied')
            eventually([], url, 403, 'A is not on the allow list')

        def allow_list_only():
            good(*group, '--clear')
            if lists() != ([], []):
                raise Failure('--clear left rules: %s' % (lists(),))
            good(*group, '--allow', addresses['b'])
            eventually(observer_b, url, 200, 'B allowed (allow list only)')
            eventually([], url, 403, 'A not on the allow list')

        def refusals():
            refused(*group, '--allow', '0.0.0.0/0')
            refused(*group, '--deny', 'not-an-address')
            if lists() != ([net(addresses['b'])], []):
                raise Failure('a refused command changed the rules: %s' % (lists(),))

        def cidr_and_clear():
            good(*group, '--clear')
            good(*group, '--deny', str(ipaddress.ip_network(addresses['a'])))
            eventually([], url, 403, 'A denied by its CIDR form')
            eventually(observer_b, url, 200, 'B unaffected by the CIDR')
            good(*group, '--clear')
            if lists() != ([], []):
                raise Failure('--clear left rules')
            eventually([], url, 200, 'A served again after --clear')

        step('baseline: both source addresses served, new resource has no rules', baseline)
        if steps[-1]['status'] != 'PASS':
            raise Failure('the baseline failed: the rule steps need two reachable observers')
        for label, function in (('deny one address: it gets 403, the other 200', deny_one),
                                ('--allow replaces only the allow list', allow_only_replaces_one_list),
                                ('deny beats allow', deny_beats_allow),
                                ('allow list alone admits only its addresses (after --clear)', allow_list_only),
                                ('--allow /0 and a malformed address are refused, rules unchanged', refusals),
                                ('a CIDR form, then --clear serves everyone', cidr_and_clear)):
            step(label, function)
    except Exception as error:  # noqa: BLE001
        steps.append({'step': 'fixture', 'status': 'FAIL', 'detail': str(error)[:400]})
    finally:
        while cleanup:
            try:
                cleanup.pop()()
            except Exception as error:  # noqa: BLE001
                print('cleanup failed:', str(error)[:200], file=sys.stderr)
    ok = bool(steps) and all(s['status'] == 'PASS' for s in steps)
    report = {'check': 'ip-rules-rows', 'row': row, 'kind': args.kind, 'status': 'PASS' if ok else 'FAIL', 'bound_seconds': args.wait, 'seconds_until_each_answer_changed': latencies, 'steps': steps}
    print(json.dumps(report, indent=1))
    if args.output:
        Path(args.output).write_text(json.dumps(report, indent=1))
    return 0 if ok else 1


if __name__ == '__main__':
    sys.exit(main())
