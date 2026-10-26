#!/usr/bin/env python3
"""F13-02: every pb byte category is recorded per path, and billing moves by exactly the charged paths.

Runs on a paired controller (a paid or free personal workspace) against one online machine of the
same account, with the controller's outgoing UDP blocked so every flow takes the hosted relay. It
drives terminal, exec, ssh, scp, rsync, sftp and a file transfer, then reads the account's own
usage and billing API (the CLI credential is read in memory, never printed) and requires:
  * each driven category shows relay bytes of at least what was driven;
  * billing usage_bytes grew by exactly the managed-relay plus edge growth of the same window
    (direct and relay bytes of an unidentified node are recorded, never debited).
Usage: usage_categories.py TARGET
"""
import argparse
import glob
import json
import os
from pathlib import Path
import pty
import select
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

HOME = Path.home()
BLOCK = 'OUTPUT ! -o lo -p udp ! --dport 53 -j REJECT'
SIZE = 1024 * 1024
CHARGED = {('relay', 'managed')}
FLOORS = {'exec': 600 * 1024, 'ssh': 3 * SIZE, 'terminal': SIZE * 3 // 4, 'file_transfer': SIZE}


def sh(command):
    return subprocess.run(command, shell=True, capture_output=True, text=True)


def pb(*arguments, timeout=150):
    return subprocess.run(['pb', *arguments], capture_output=True, text=True, timeout=timeout, stdin=subprocess.DEVNULL)


class Api:
    def __init__(self):
        self.token = self.read_token()
        self.server = json.loads((HOME / '.config/paperboat/config.json').read_text())['server_url']

    @staticmethod
    def read_token():
        # Any pb command that reaches the server renews the saved access credential first.
        pb('machine', 'list', '--json')
        secrets = glob.glob(str(HOME / '.config/paperboat/credentials/secrets/profile-*-access.secret'))
        return Path(secrets[0]).read_text().strip()

    def get(self, path):
        for attempt in range(2):
            request = urllib.request.Request(self.server + path, headers={'Authorization': 'Bearer ' + self.token,
                                                                          'X-Paperboat-Workspace': 'personal'})
            try:
                with urllib.request.urlopen(request, timeout=30) as response:
                    return json.loads(response.read())['data']
            except urllib.error.HTTPError as error:
                if error.code != 401 or attempt:
                    raise
                self.token = self.read_token()

    def snapshot(self, start):
        query = urllib.parse.urlencode({'start': start, 'end': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime(time.time() + 60)),
                                        'limit': '200', 'workspace': 'personal'})
        usage = self.get('/v1/usage?' + query)
        billing = self.get('/v1/billing/workspaces/personal')
        charged = sum(p['upload_bytes'] + p['download_bytes'] for p in usage['totals']['by_path']
                      if (p['mode'], p['hosting']) in CHARGED or p['mode'] == 'edge')
        kinds = {}
        for resource in usage['resources']:
            for path in resource['paths']:
                if path['mode'] == 'relay':
                    kinds[resource['resource_kind']] = kinds.get(resource['resource_kind'], 0) + path['upload_bytes'] + path['download_bytes']
        return {'billing': billing['usage_bytes'], 'charged': charged, 'kinds': kinds}

    def quiet_snapshot(self, start, before=None):
        """Reads unchanged for SETTLE seconds (and, after a drive, every category reached): nothing is mid-flight.

        A recorder batches changed counters to one report per 5 minutes, so a driven flow shows up up to 5 minutes
        later and only reads unchanged for longer than that prove nothing is pending."""
        previous, since = None, time.monotonic()
        for _ in range(60):
            current = self.snapshot(start)
            reached = before is None or all(current['kinds'].get(k, 0) - before['kinds'].get(k, 0) >= floor
                                            for k, floor in FLOORS.items())
            if current != previous:
                previous, since = current, time.monotonic()
            elif reached and time.monotonic() - since >= SETTLE:
                return current
            time.sleep(20)
        raise RuntimeError('usage did not settle: another workload is using this account or a category was not recorded')


SETTLE = 330  # the recorder batch interval (5 min) plus a margin


def block_udp():
    sh('for t in iptables ip6tables; do sudo -n $t -I %s; done; (sleep 600; for t in iptables ip6tables; do sudo -n $t -D %s; done) >/dev/null 2>&1 &' % (BLOCK, BLOCK))


def unblock_udp():
    sh('for t in iptables ip6tables; do sudo -n $t -D %s 2>/dev/null; done; true' % BLOCK)


def wait_for_relay(target):
    end = time.time() + 120
    while time.time() < end:
        report = json.loads(pb('ping', target, '--json').stdout)
        report = report.get('data', report)
        paths = {s['path'] for s in report['samples'] if not s['lost']}
        if paths and 'direct' not in paths:
            return
        time.sleep(3)
    raise RuntimeError('the flow never left the direct path')


def terminal(target, name):
    marker = 'DONE_$((1+1))'
    child, descriptor = pty.fork()
    if child == 0:
        os.execvp('pb', ['pb', 'connect', target, 'new', '--name', name])
    output = bytearray()

    def read_until(condition, seconds):
        end = time.time() + seconds
        while time.time() < end and not condition(output):
            if select.select([descriptor], [], [], .3)[0]:
                try:
                    output.extend(os.read(descriptor, 65536))
                except OSError:
                    return

    read_until(lambda out: b'$' in out and target.encode() in out, 60)
    os.write(descriptor, ('head -c %d /dev/urandom | base64; echo %s\r' % (SIZE * 3 // 4, marker)).encode())
    read_until(lambda out: out.endswith(b'DONE_2\r\n') or b'\nDONE_2\r\n' in out[-40:], 90)
    os.write(descriptor, b'exit\r')
    time.sleep(2)
    if b'DONE_2' not in output:
        raise RuntimeError('terminal output did not arrive over the relay')


def drive(target, work):
    data = work / 'data.bin'
    data.write_bytes(os.urandom(SIZE))
    remote = '/tmp/usage-categories-' + str(os.getpid())
    steps = {
        'exec': lambda: pb('exec', target, '--timeout', '90s', '--', 'sh', '-c', 'head -c %d /dev/urandom | base64' % (SIZE // 2)),
        'ssh': lambda: pb('ssh', target, '--', 'sh -c "head -c %d /dev/urandom | base64"' % SIZE),
        'scp': lambda: pb('scp', str(data), target + ':' + remote + '.scp'),
        'rsync': lambda: pb('rsync', '-a', str(data), target + ':' + remote + '.rsync'),
        'sftp': lambda: pb('sftp', '-b', str(work / 'sftp.batch'), target),
        'send': lambda: pb('send', str(data), '--to', target, '--json'),
    }
    (work / 'sftp.batch').write_text('put %s %s.sftp\nrm %s.sftp\n' % (data, remote, remote))
    for name, step in steps.items():
        result = step()
        if result.returncode != 0:
            raise RuntimeError('%s failed over the relay: %s' % (name, result.stderr.strip()[-160:]))
    terminal(target, 'usage-cat-%d' % os.getpid())
    pb('exec', target, '--', 'sh', '-c', 'rm -f %s.*' % remote)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    target = parser.parse_args().target
    api = Api()
    start = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime(time.time() - 3600))
    work = Path('/tmp/usage-categories-%d' % os.getpid())
    work.mkdir()
    ok, detail = False, {}
    try:
        before = api.quiet_snapshot(start)
        block_udp()
        try:
            wait_for_relay(target)
            drive(target, work)
        finally:
            unblock_udp()
        try:
            after = api.quiet_snapshot(start, before)
        except RuntimeError:
            after = api.snapshot(start)
        grew = {kind: after['kinds'].get(kind, 0) - before['kinds'].get(kind, 0) for kind in FLOORS}
        missing = [kind for kind in grew if grew[kind] < FLOORS[kind]]
        billed, charged = after['billing'] - before['billing'], after['charged'] - before['charged']
        detail = {'relay_bytes_by_category': grew, 'billing_delta': billed, 'charged_path_delta': charged}
        ok = not missing and billed == charged
        if missing:
            detail['categories_below_driven'] = missing
    finally:
        sh('rm -rf %s' % work)
    print(json.dumps({'check': 'usage-categories', 'target': target, 'status': 'PASS' if ok else 'FAIL', **detail}), flush=True)
    sys.exit(0 if ok else 1)


if __name__ == '__main__':
    main()
