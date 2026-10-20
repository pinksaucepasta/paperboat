#!/usr/bin/env python3
"""F9-04: a team member reaches a shared machine's private TCP ports only with permission, and
losing the permission ends the flows already open.

Two accounts, each with its own signed-in daemon: the OWNER machine (account A) is shared into a
team that a second account B is a member of, and B's machine runs `pb access machine` against it.
Run from any host that can ssh to both:

  python3 private_access_permission.py --owner pb@pbvm2 --owner-alias pbvm2-4 \
      --accessor pujan@ubuntu24 --team paperboat-smoke-paid

A member reaches the ports only when the machine is shared into the team AND B's effective grant
includes private_access AND the team setting allow_private_access is on. Phases, each asserted:
  1 setting off, grant given            -> denied, the message says private access is turned off
  2 setting on, grant without private_access -> denied, the message names the missing permission
  3 setting on, grant with private_access -> allowed, a flow is held open
  4 setting turned off                  -> the held flow ends; a new attempt is denied (turned off)
  5 setting on again, flow held, then the grant is revoked -> the flow ends; denied (permission)
The setting and the sharing are restored at the end whatever happened.
"""
import argparse
import base64
import json
import shlex
import subprocess
import sys
import time
import uuid

ORIGIN = r'''
import socketserver, threading, time, sys
class Stream(socketserver.BaseRequestHandler):
    def handle(self):
        self.request.settimeout(300)
        while True:
            chunk = self.request.recv(4096)
            if not chunk:
                return
            self.request.sendall(chunk)
socketserver.ThreadingTCPServer.daemon_threads = True
server = socketserver.ThreadingTCPServer(('127.0.0.1', 0), Stream)
threading.Thread(target=server.serve_forever, daemon=True).start()
print(server.server_address[1], flush=True)
time.sleep(float(sys.argv[1]))
'''

# Runs on the accessor: start `pb access machine`; print its listener (or its refusal), then
# optionally hold one echo flow open and log it until it ends.
HOLDER = r'''
import json, os, select, signal, socket, subprocess, sys, time
alias, port, team, hold_file, hold = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5] == 'hold'
open(hold_file + '.pid', 'w').write(str(os.getpid()))
child = subprocess.Popen(['pb', 'access', 'machine', alias, '--port', port, '--workspace', team, '--listen', '127.0.0.1:0', '--json'],
                         stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
out, end = b'', time.time() + 70
while b'\n' not in out:
    if child.poll() is not None and not select.select([child.stdout], [], [], 0)[0] or time.time() > end:
        print('REFUSED', out.decode(errors='replace').strip()[:600] or 'no output'); sys.exit(0)
    if select.select([child.stdout], [], [], .2)[0]:
        out += os.read(child.stdout.fileno(), 4096)
line = out.split(b'\n', 1)[0]
try:
    receipt = json.loads(line)
except ValueError:
    print('REFUSED', line.decode(errors='replace')[:600]); sys.exit(0)
if receipt.get('ok') is False:
    print('REFUSED', receipt['error'].get('message', '')); sys.exit(0)
receipt = receipt.get('data', receipt)
host, local = receipt['listen_address'].rsplit(':', 1)
print('LISTENER', host, local, flush=True)
if not hold:
    try:
        connection = socket.create_connection((host, int(local)), timeout=20)
        connection.settimeout(20)
        connection.sendall(b'ping')
        print('ECHO_OK' if connection.recv(4) == b'ping' else 'NO_ECHO', flush=True)
    except OSError as error:
        print('NO_ECHO', type(error).__name__, flush=True)
    child.send_signal(signal.SIGTERM)
    sys.exit(0)
connection = socket.create_connection((host, int(local)), timeout=20)
connection.settimeout(2)
with open(hold_file, 'w') as log:
    log.write('start %f\n' % time.time()); log.flush()
    while True:
        try:
            connection.sendall(b'ping')
            if connection.recv(4) != b'ping':
                log.write('end %f closed\n' % time.time()); break
            log.write('alive %f\n' % time.time()); log.flush()
        except socket.timeout:
            log.write('alive %f\n' % time.time()); log.flush()
        except OSError as error:
            log.write('end %f %s\n' % (time.time(), type(error).__name__)); break
        time.sleep(.5)
child.send_signal(signal.SIGTERM)
'''

# Runs on the owner: change the team's allow_private_access setting through the team mutation.
SETTING = r'''
import glob, json, os, sys, urllib.parse, urllib.request, uuid
home = os.path.expanduser('~')
token = open(glob.glob(home + '/.config/paperboat/credentials/secrets/profile-*-access.secret')[0]).read().strip()
server = json.load(open(home + '/.config/paperboat/config.json'))['server_url']
team, value = sys.argv[1], sys.argv[2] == 'on'
def call(method, path, body=None, key=None):
    headers = {'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json'}
    if key:
        headers['Idempotency-Key'] = key
    request = urllib.request.Request(server + path, method=method, headers=headers,
                                     data=json.dumps(body).encode() if body is not None else None)
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.loads(response.read())
quoted = urllib.parse.quote(team)
current = call('GET', '/v1/teams/' + quoted)
current = current.get('data', current)
if sys.argv[2] == 'show':
    print(json.dumps({'allow_private_access': bool(current.get('allow_private_access'))}))
    sys.exit(0)
operation = 'operation_' + uuid.uuid4().hex
call('POST', '/v1/teams/' + quoted + '/actions', {'operation_id': operation, 'expected_generation': current['generation'],
     'action': 'private_access', 'allow_private_access': value}, operation)
print(json.dumps({'allow_private_access': value}))
'''


def ssh(destination, command, timeout=180, stdin=None):
    return subprocess.run(['ssh', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=20', destination,
                           'export PATH=$HOME/.local/bin:$PATH; ' + command],
                          capture_output=True, text=True, timeout=timeout, input=stdin)


def pb(destination, *arguments, timeout=180):
    return ssh(destination, ' '.join(shlex.quote(a) for a in ('pb', *arguments)), timeout=timeout)


def pb_json(destination, *arguments):
    result = pb(destination, *arguments, '--json')
    assert result.returncode == 0, result.stdout[-200:] + result.stderr[-200:]
    value = json.loads(result.stdout)
    return value.get('data', value) if isinstance(value, dict) else value


def eventually(what, check, seconds=90):
    end = time.time() + seconds
    while True:
        value = check()
        if value:
            return value
        assert time.time() < end, 'timed out waiting for ' + what
        time.sleep(2)


def encoded(script):
    return base64.b64encode(script.encode()).decode()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--owner', required=True)
    parser.add_argument('--owner-alias', required=True)
    parser.add_argument('--accessor', required=True)
    parser.add_argument('--team', required=True)
    args = parser.parse_args()
    owner, accessor, team = args.owner, args.accessor, args.team
    machine = next(m for m in pb_json(owner, 'machine', 'list')['machines'] if m['alias'] == args.owner_alias)
    member = pb_json(accessor, 'auth', 'status')['account']['id']
    results = {}
    log = '/tmp/pb-perm-' + uuid.uuid4().hex[:8]

    def exec_script(destination, script, *arguments):
        command = 'python3 -c ' + shlex.quote('import base64,sys;exec(base64.b64decode(%r).decode())' % encoded(script))
        return command + ' ' + ' '.join(shlex.quote(a) for a in arguments)

    def setting(value):
        result = ssh(owner, exec_script(owner, SETTING, team, value))
        assert result.returncode == 0, result.stdout[-300:] + result.stderr[-300:]
        return json.loads(result.stdout.strip().splitlines()[-1])['allow_private_access']

    def generation():
        return str(pb_json(owner, 'team', 'get', team)['generation'])

    def grant(*capabilities):
        pb_json(owner, 'team', 'machine', 'grant', team, machine['id'], '--member', member,
                '--capability', ','.join(capabilities), '--generation', generation())

    def attempt(port, hold=False, seconds=100):
        """Run pb access machine on the accessor. Returns (listener published, refusal message)."""
        mode = 'hold' if hold else 'once'
        command = exec_script(accessor, HOLDER, args.owner_alias, port, team, log + '.hold', mode)
        if hold:
            ssh(accessor, f'rm -f {log}.hold; (setsid nohup {command} > {log}.holder 2>&1 < /dev/null &)')
            return True, ''
        result = ssh(accessor, f'timeout {seconds} {command}', timeout=seconds + 30)
        lines = result.stdout.strip().splitlines()
        first = lines[0] if lines else ''
        if first.startswith('LISTENER'):
            return 'ECHO_OK' in lines[1:], ' '.join(lines[1:]) or 'no echo'
        return False, first[len('REFUSED '):] if first.startswith('REFUSED') else first

    def refused_with(port, needle, seconds=90):
        """The refusal message appears once the daemon's topology reflects the new setting or grant."""
        message = {'text': ''}

        def check():
            ok, text = attempt(port, seconds=70)
            message['text'] = text
            return (not ok) and needle in text
        try:
            eventually('a refusal mentioning ' + repr(needle), check, seconds)
            return True, message['text']
        except AssertionError:
            return False, message['text']

    def held_flow_alive():
        return 'alive' in ssh(accessor, f'cat {log}.hold 2>/dev/null').stdout

    def held_flow_started(what):
        try:
            eventually(what, held_flow_alive, 90)
        except AssertionError:
            holder = ssh(accessor, f'cat {log}.holder {log}.hold 2>&1 | head -c 600').stdout
            raise AssertionError(what + ' never carried traffic; holder said: ' + holder) from None

    def held_flow_end():
        lines = ssh(accessor, f'cat {log}.hold 2>/dev/null').stdout.splitlines()
        return next((line for line in lines if line.startswith('end')), None)

    original = None
    script = encoded(ORIGIN)
    ssh(owner, f'(setsid nohup python3 -c {shlex.quote("import base64,sys;exec(base64.b64decode(%r).decode())" % script)} 1500 > {log}.out 2>&1 < /dev/null & echo $! > {log}.pid)')
    try:
        port = str(json.loads(eventually('origin', lambda: ssh(owner, f'head -1 {log}.out').stdout.strip() or None, 30)))
        original = json.loads(ssh(owner, exec_script(owner, SETTING, team, 'show')).stdout.strip().splitlines()[-1])['allow_private_access']
        pb_json(owner, 'team', 'machine', 'share', team, machine['id'], '--generation', generation())

        setting('off')
        grant('terminal', 'private_access')
        results['setting_off'], results['setting_off_message'] = refused_with(port, 'turned off')

        setting('on')
        grant('terminal')
        results['no_private_access'], results['no_private_access_message'] = refused_with(port, 'private_access')

        grant('terminal', 'private_access')
        results['allowed'] = eventually('an allowed flow', lambda: attempt(port, seconds=70)[0], 120)
        attempt(port, hold=True)
        held_flow_started('the held flow')

        turned_off = time.time()
        setting('off')
        end = eventually('the held flow to end after the setting was turned off', held_flow_end, 90)
        results['flow_ended_after_setting_off_seconds'] = round(float(end.split()[1]) - turned_off, 1)
        results['denied_after_setting_off'], results['denied_after_setting_off_message'] = refused_with(port, 'turned off')

        setting('on')
        eventually('an allowed flow again', lambda: attempt(port, seconds=70)[0], 120)
        attempt(port, hold=True)
        held_flow_started('the second held flow')
        revoked = time.time()
        grant('terminal')
        end = eventually('the held flow to end after the grant was revoked', held_flow_end, 90)
        results['flow_ended_after_revoke_seconds'] = round(float(end.split()[1]) - revoked, 1)
        results['denied_after_revoke'], results['denied_after_revoke_message'] = refused_with(port, 'private_access')
    finally:
        try:
            if original is not None:
                setting('on' if original else 'off')
        finally:
            try:
                pb(owner, 'team', 'machine', 'unshare', team, machine['id'], '--generation', generation())
            finally:
                ssh(accessor, f'kill $(cat {log}.hold.pid) 2>/dev/null; rm -f {log}.hold {log}.hold.pid {log}.holder')
                ssh(owner, f'kill $(cat {log}.pid) 2>/dev/null; rm -f {log}.out {log}.pid')
    ok = (results.get('setting_off') and results.get('no_private_access') and results.get('allowed') and
          results.get('denied_after_setting_off') and results.get('denied_after_revoke') and
          results.get('flow_ended_after_setting_off_seconds', 999) <= 60 and
          results.get('flow_ended_after_revoke_seconds', 999) <= 60)
    print(json.dumps({'check': 'private-access-permission', 'status': 'PASS' if ok else 'FAIL', **results}), flush=True)
    sys.exit(0 if ok else 1)


if __name__ == '__main__':
    main()
