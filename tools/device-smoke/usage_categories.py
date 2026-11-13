#!/usr/bin/env python3
"""F13-02: every pb byte category is recorded per path, and billing moves by exactly the charged paths.

Runs on a paired controller (a paid or free personal workspace) against one online machine of the
same account, with the controller's outgoing UDP blocked so every flow takes the hosted relay. It
drives terminal, exec, ssh, scp, rsync, sftp and a file transfer, then reads the account's own
usage and billing API (the CLI credential is read in memory, never printed) and requires:
  * each driven category shows relay bytes of at least what was driven;
  * billing usage_bytes grew by exactly the managed-relay plus edge growth of the same window
    (direct and relay bytes of an unidentified node are recorded, never debited).
Usage: usage_categories.py TARGET [--no-ssh] [--team TEAM --driver ACCOUNT [--idle ACCOUNT ...]]

--private-port PORT drives private access too: `pb access machine` forwards a loopback port to PORT on TARGET (an HTTP
server serving /blob.bin, 1 MiB or more) and the blob is fetched through it (the machine_service category).
--no-ssh leaves out ssh, scp, rsync and sftp (the ssh category): an account at its managed-SSH client capacity
(doctor code ssh_client_key_capacity) cannot start them.

With --team the machine is team-owned and the numbers are the team's: the pool (the team's usage and billing
usage_bytes, billed by exactly the charged paths) and per member (usage filtered by actor_account_id): the driving
member's bytes equal the team's, every --idle member's bytes did not move.

With PB_SMOKE_WINDOWS_PB set the controller is the Windows device (winhost.py; TARGET is a Unix machine of the same
account): its UDP is blocked by a firewall rule, pb.exe drives every category (no rsync on Windows: scp and sftp stand
for the ssh category), and the account's API credential, which Windows keeps under DPAPI where this controller cannot
read it, comes from PB_SMOKE_API_CMD (also on Unix, for a controller with no user sign-in, such as a container: a team-scoped API token): a command that prints the access token and the server URL on two lines (for
example ssh to a Unix machine signed in to the same account).
"""
import argparse
import contextlib
import glob
import json
import shlex

import cli_credential
import os
import winhost
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
DEVICE = winhost.device()
PB = winhost.pb_path()
BLOCK = 'OUTPUT ! -o lo -p udp ! --dport 53 -j REJECT'
SIZE = 1024 * 1024
CHARGED = {('relay', 'managed')}
FLOORS = {'exec': 600 * 1024, 'ssh': 3 * SIZE, 'terminal': SIZE * 3 // 4, 'file_transfer': SIZE}


def sh(command):
    return subprocess.run(command, shell=True, capture_output=True, text=True)


WORKSPACE = ['personal']  # the team of --team: the older official pb names a team machine only with --workspace


def pb(*arguments, timeout=150):
    if WORKSPACE[0] != 'personal':
        arguments = ('--workspace', WORKSPACE[0], *arguments)
    if DEVICE:
        return DEVICE.run([DEVICE.pb_exe, *arguments], timeout=timeout)
    return subprocess.run(['pb', *arguments], capture_output=True, text=True, timeout=timeout, stdin=subprocess.DEVNULL)


def credential():
    """(access token, server URL) of the account: this machine's CLI credential, or PB_SMOKE_API_CMD for the device."""
    if os.environ.get('PB_SMOKE_API_CMD'):
        lines = subprocess.run(shlex.split(os.environ['PB_SMOKE_API_CMD']), capture_output=True, text=True, timeout=60,
                               stdin=subprocess.DEVNULL).stdout.split()
        assert len(lines) == 2, 'PB_SMOKE_API_CMD must print the access token and the server URL'
        return lines[0], lines[1]
    return cli_credential.access_token(), cli_credential.server_url()


class Api:
    def __init__(self, workspace='personal'):
        self.workspace = workspace
        self.token, self.server = self.read_token(), credential()[1]

    @staticmethod
    def read_token():
        # Any pb command that reaches the server renews the saved access credential first.
        pb('machine', 'list', '--json')
        return credential()[0]

    def get(self, path):
        for attempt in range(4):
            request = urllib.request.Request(self.server + path, headers={'Authorization': 'Bearer ' + self.token,
                                                                          'X-Paperboat-Workspace': self.workspace})
            try:
                with urllib.request.urlopen(request, timeout=30) as response:
                    return json.loads(response.read())['data']
            except urllib.error.HTTPError as error:
                if error.code != 401 or attempt:
                    raise
                self.token = self.read_token()
            except (urllib.error.URLError, TimeoutError):
                # A handshake or read timeout of the control-plane API during a long run: retry, then give up.
                if attempt == 3:
                    raise
                time.sleep(10)

    def snapshot(self, start, actors=()):
        end = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime(time.time() + 60))

        def usage_of(actor=''):
            query = {'start': start, 'end': end, 'limit': '200', 'workspace': self.workspace}
            if actor:
                query['actor_account_id'] = actor
            return self.get('/v1/usage?' + urllib.parse.urlencode(query))
        usage = usage_of()
        billing = self.get('/v1/billing/workspaces/' + ('personal' if self.workspace == 'personal' else 'teams/' + self.workspace))
        charged = sum(p['upload_bytes'] + p['download_bytes'] for p in usage['totals']['by_path']
                      if (p['mode'], p['hosting']) in CHARGED or p['mode'] == 'edge')
        kinds = {}
        for resource in usage['resources']:
            for path in resource['paths']:
                if path['mode'] == 'relay':
                    kinds[resource['resource_kind']] = kinds.get(resource['resource_kind'], 0) + path['upload_bytes'] + path['download_bytes']
        total = lambda u: sum(p['upload_bytes'] + p['download_bytes'] for p in u['totals']['by_path'])
        return {'billing': billing['usage_bytes'], 'charged': charged, 'kinds': kinds, 'total': total(usage),
                'actors': {actor: total(usage_of(actor)) for actor in actors}}

    def quiet_snapshot(self, start, before=None, actors=()):
        """Reads unchanged for SETTLE seconds (and, after a drive, every category reached): nothing is mid-flight.

        A recorder batches changed counters to one report per 5 minutes, so a driven flow shows up up to 5 minutes
        later and only reads unchanged for longer than that prove nothing is pending."""
        previous, since = None, time.monotonic()
        for _ in range(60):
            current = self.snapshot(start, actors)
            reached = before is None or all(current['kinds'].get(k, 0) - before['kinds'].get(k, 0) >= floor
                                            for k, floor in FLOORS.items())
            if current != previous:
                previous, since = current, time.monotonic()
            elif reached and time.monotonic() - since >= SETTLE:
                return current
            time.sleep(20)
        raise RuntimeError('usage did not settle: another workload is using this account or a category was not recorded')


SETTLE = 330  # the recorder batch interval (5 min) plus a margin


PF_ANCHOR = 'com.apple/pbusage'  # macOS: evaluated by the stock pf.conf (anchor "com.apple/*")


def mac_sudo(command):
    # A Mac controller has no passwordless sudo: the password comes from SUDO_PW_FILE (a path on the Mac).
    return "sudo -S -p '' sh -c %s < %s" % (shlex.quote(command), shlex.quote(os.environ['SUDO_PW_FILE']))


def block_udp(stack):
    if DEVICE:
        stack.enter_context(DEVICE.udp_blocked())
        return
    if cli_credential.DARWIN:
        sh(mac_sudo('printf "block drop out quick proto udp from any to any port != 53\\n" | pfctl -a %s -f - 2>/dev/null; '
                    'pfctl -E >/dev/null 2>&1; pfctl -F states >/dev/null 2>&1; (sleep 600; pfctl -a %s -F all) >/dev/null 2>&1 &'
                    % (PF_ANCHOR, PF_ANCHOR)))
        return
    sh('for t in iptables ip6tables; do sudo -n $t -I %s; done; (sleep 600; for t in iptables ip6tables; do sudo -n $t -D %s; done) >/dev/null 2>&1 &' % (BLOCK, BLOCK))


def unblock_udp(stack):
    if DEVICE:
        stack.close()
        return
    if cli_credential.DARWIN:
        sh(mac_sudo('pfctl -a %s -F all >/dev/null 2>&1; true' % PF_ANCHOR))
        return
    sh('for t in iptables ip6tables; do sudo -n $t -D %s 2>/dev/null; done; true' % BLOCK)


def wait_for_relay(target):
    end = time.time() + 120
    while time.time() < end:
        report = json.loads(pb('ping', target, '--json').stdout)
        report = report.get('data', report)
        if 'samples' not in report:
            # A team machine refuses ping (permission_denied) to a member with exec access: with UDP blocked the
            # direct path ends by itself; the relay bytes recorded per category prove the path afterwards.
            time.sleep(45)
            return
        paths = {s['path'] for s in report['samples'] if not s['lost']}
        if paths and 'direct' not in paths:
            return
        time.sleep(3)
    raise RuntimeError('the flow never left the direct path')


def terminal(target, name, workspace='personal'):
    marker = 'DONE_$((1+1))'
    child, descriptor = pty.fork()
    if child == 0:
        argv = winhost.terminal_argv(PB, ['--workspace', workspace, 'connect', target, 'new', '--name', name])
        os.execvpe(argv[0], argv, dict(os.environ, **winhost.terminal_env(PB)))
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


SSH_STEPS = ('ssh', 'scp', 'rsync', 'sftp')


FETCH_BLOB = r'''
$ok = $false
for ($i = 0; $i -lt 30 -and -not $ok; $i++) {
  Start-Sleep 2
  try { $r = Invoke-WebRequest -UseBasicParsing -Uri http://127.0.0.1:18081/blob.bin -TimeoutSec 60; if ($r.RawContentLength -ge $req.size) { $ok = $true } } catch {}
}
if (-not $ok) { throw 'private access never served the blob' }
'''


def private_access(target, port, workspace):
    """One fetch of /blob.bin through `pb access machine` (private TCP to PORT on target, over the relay)."""
    arguments = [*(['--workspace', workspace] if workspace != 'personal' else []), 'access', 'machine', target,
                 '--port', str(port), '--listen', '127.0.0.1:18081']
    if DEVICE:
        # The forward and the fetch both run on the Windows device: its loopback is where pb listens.
        with DEVICE.temp_dir('pb-access') as directory:
            pid = DEVICE.start([DEVICE.pb_exe, *arguments], directory)
            try:
                DEVICE.ps('private access fetch', FETCH_BLOB, timeout=240, size=SIZE)
            finally:
                DEVICE.stop(pid)
        return
    forward = subprocess.Popen([str(PB), *arguments], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        for _ in range(30):
            time.sleep(2)
            try:
                if len(urllib.request.urlopen('http://127.0.0.1:18081/blob.bin', timeout=60).read()) >= SIZE:
                    return
            except OSError:
                continue
        raise RuntimeError('private access never served the blob')
    finally:
        forward.terminate()
        forward.wait()


def drive(target, work, skip=(), workspace='personal', private_port=0):
    data = work / 'data.bin'
    if DEVICE:
        data.write_random(SIZE)
    else:
        data.write_bytes(os.urandom(SIZE))
    remote = '/tmp/usage-categories-' + str(os.getpid())
    steps = {
        'exec': lambda: pb('exec', target, '--timeout', '90s', '--', 'sh', '-c', 'head -c %d /dev/urandom | base64' % (SIZE // 2)),
        'ssh': lambda: pb('ssh', target, '--', 'sh -c "head -c %d /dev/urandom | base64"' % SIZE),
        'scp': lambda: pb('scp', str(data), target + ':' + remote + '.scp'),
        'rsync': lambda: pb('rsync', '-a', str(data), target + ':' + remote + '.rsync'),  # not on Windows
        'sftp': lambda: pb('sftp', '-b', str(work / 'sftp.batch'), target),
        'send': lambda: pb('send', str(data), '--to', target, '--json'),
    }
    (work / 'sftp.batch').write_text('put %s %s.sftp\nrm %s.sftp\n' % (data, remote, remote))
    for name, step in steps.items():
        if DEVICE and name == 'rsync' or name in skip:
            continue
        result = step()
        if result.returncode != 0:
            raise RuntimeError('%s failed over the relay: %s' % (name, result.stderr.strip()[-160:]))
    terminal(target, 'usage-cat-%d' % os.getpid(), workspace)
    if private_port:
        private_access(target, private_port, workspace)
    pb('exec', target, '--', 'sh', '-c', 'rm -f %s.*' % remote)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--no-ssh', action='store_true', help='leave out the ssh category (ssh, scp, rsync, sftp)')
    parser.add_argument('--private-port', type=int, default=0, help='also drive private access to this port on TARGET')
    parser.add_argument('--team', default='personal', help='the team that owns TARGET (default: the personal workspace)')
    parser.add_argument('--driver', default='', help='account id of the member that drives the traffic (with --team)')
    parser.add_argument('--idle', action='append', default=[], help='account id of a member that drives nothing (with --team)')
    arguments = parser.parse_args()
    target, team = arguments.target, arguments.team
    WORKSPACE[0] = team
    assert team == 'personal' or arguments.driver, '--team needs --driver ACCOUNT_ID'
    skip = SSH_STEPS if arguments.no_ssh else ()
    if arguments.no_ssh:
        FLOORS.pop('ssh')
    if arguments.private_port:
        FLOORS['machine_service'] = SIZE
    actors = [arguments.driver, *arguments.idle] if team != 'personal' else []
    api = Api(team)
    start = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime(time.time() - 3600))
    scratch = winhost.workdir('usage-categories-')  # on the device for a Windows controller
    work = scratch.__enter__()
    ok, detail = False, {}
    blocked = contextlib.ExitStack()
    try:
        before = api.quiet_snapshot(start, actors=actors)
        block_udp(blocked)
        try:
            wait_for_relay(target)
            drive(target, work, skip, team, arguments.private_port)
        finally:
            unblock_udp(blocked)
        try:
            after = api.quiet_snapshot(start, before, actors)
        except RuntimeError:
            after = api.snapshot(start, actors)
        grew = {kind: after['kinds'].get(kind, 0) - before['kinds'].get(kind, 0) for kind in FLOORS}
        missing = [kind for kind in grew if grew[kind] < FLOORS[kind]]
        billed, charged = after['billing'] - before['billing'], after['charged'] - before['charged']
        detail = {'all_relay_kinds_delta': {k: after['kinds'][k] - before['kinds'].get(k, 0) for k in after['kinds']}, 'relay_bytes_by_category': grew, 'billing_delta': billed, 'charged_path_delta': charged}
        ok = not missing and billed == charged
        if actors:
            moved = {actor: after['actors'][actor] - before['actors'][actor] for actor in actors}
            detail.update({'team': team, 'team_total_delta': after['total'] - before['total'], 'member_bytes_delta': moved})
            # Per member: all of the window's bytes are the driver's, and an idle member's bytes did not move.
            ok = ok and moved[arguments.driver] == detail['team_total_delta'] and all(moved[a] == 0 for a in arguments.idle)
        if missing:
            detail['categories_below_driven'] = missing
    finally:
        scratch.__exit__(None, None, None)
    print(json.dumps({'check': 'usage-categories', 'target': target, 'status': 'PASS' if ok else 'FAIL', **detail}), flush=True)
    sys.exit(0 if ok else 1)


if __name__ == '__main__':
    main()
