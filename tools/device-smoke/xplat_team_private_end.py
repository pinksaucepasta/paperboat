#!/usr/bin/env python3
"""X-19: held private TCP flows end on grant revoke and team setting off.

Owner fleet is XPLAT_* (including M); member L/W are XPE_MEMBER_*.
For Mac clients, invert the two-account fixture as X-21 does: Y owns L/W,
F approves their shares and the fleet Mac is the member, driven via pb exec.
XPE_MEMBER_L_HOST/USER/UID/DOCKER describe its Linux container independently.
XPE_TEAM must name a throwaway team owned by the owner account. Only its
setting, exact machine shares and member grants are mutated. Run --pairs A>B.
Origins run through another device in their owner's account via pb exec,
so grant changes cannot kill the origin. The holder runs ON the member device.
"""
import argparse
import concurrent.futures
import json
import math
import os
import sys
from pathlib import Path
import time
import uuid

import xplat_access
import xplat_fleet as fleet
import xplat_team

ORIGIN = r'''
import os, socketserver, sys, threading, time
stop, port = sys.argv[1:]
class Echo(socketserver.BaseRequestHandler):
    def handle(self):
        self.request.settimeout(3)
        try:
            while True:
                b = self.request.recv(4096)
                if not b: break
                self.request.sendall(b)
        except OSError: pass
server = socketserver.ThreadingTCPServer(('127.0.0.1', int(port)), Echo)
server.daemon_threads = True
threading.Thread(target=server.serve_forever, daemon=True).start()
try:
    end = time.monotonic() + 480
    while not os.path.exists(stop) and time.monotonic() < end: time.sleep(.2)
finally:
    server.shutdown(); server.server_close()
    if os.path.exists(stop): os.unlink(stop)
'''

HOLDER = r'''
import json, os, socket, subprocess, sys, threading, time
pb, alias, team, port, state, stop = sys.argv[1:]
child = subprocess.Popen([pb, 'access', 'machine', '--workspace', team, alias,
    '--port', port, '--listen', '127.0.0.1:0', '--json'], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
lines = []
threading.Thread(target=lambda: lines.append(child.stdout.readline()), daemon=True).start()
def publish(**value):
    # Atomic replacement: the controller must never read a partially written status.
    with open(state + '.tmp', 'w') as f: json.dump(value, f)
    os.replace(state + '.tmp', state)
try:
    end = time.monotonic() + 60
    while not lines and time.monotonic() < end and child.poll() is None: time.sleep(.1)
    receipt = json.loads(lines[0]) if lines else {}
    data = receipt.get('data', receipt)
    if not data.get('listen_address'):
        publish(error='access did not publish listener', receipt=receipt)
    else:
        host, port = data['listen_address'].rsplit(':', 1)
        connection = socket.create_connection((host, int(port)), timeout=10)
        connection.settimeout(2)
        count, end = 0, time.monotonic() + 180
        with connection:
            while not os.path.exists(stop) and time.monotonic() < end:
                try:
                    connection.sendall(b'rx19t-echo')
                    got = b''
                    while len(got) < 10:
                        b = connection.recv(10 - len(got))
                        if not b: break
                        got += b
                    if got != b'rx19t-echo':
                        publish(ended=True, echoes=count, reason='EOF'); break
                    count += 1
                    publish(alive=True, echoes=count)
                except socket.timeout:
                    # Silence is not closure and must never produce a passing result.
                    publish(alive=True, echoes=count, stalled=True)
                except OSError as error:
                    publish(ended=True, echoes=count, reason=type(error).__name__); break
                time.sleep(.25)
            else: publish(error='holder stopped without connection closure', echoes=count)
except Exception as error:
    publish(error=type(error).__name__ + ': ' + str(error))
finally:
    if child.poll() is None: child.terminate()
    try: child.wait(5)
    except subprocess.TimeoutExpired: child.kill(); child.wait()
'''


def closure_seconds(state, started, observed):
    """Only a previously flowing connection's observed EOF/reset proves closure."""
    elapsed = observed - started
    assert state.get('ended') and state.get('echoes', 0) >= 2, 'no held-flow closure: %r' % state
    assert math.isfinite(elapsed) and 0 <= elapsed <= 30, 'closure outside 30 seconds: %r' % elapsed
    return round(elapsed, 2)


def read_state(dev, path):
    done = dev.run([xplat_access.py(dev), '-c',
                    'import pathlib,sys; p=pathlib.Path(sys.argv[1]); print(p.read_text() if p.exists() else "{}")', path])
    assert done.rc == 0, 'holder status read failed'
    return json.loads(done.text)


def wait_state(dev, path, predicate, seconds):
    deadline = time.monotonic() + seconds
    while True:
        state = read_state(dev, path)
        assert not state.get('error'), str(state)
        if predicate(state): return state
        assert time.monotonic() < deadline, 'holder deadline: %r' % state
        time.sleep(.3)


def run_pair(team, owner, member, oid, records, origin_controller):
    original = fleet.json_pb(team.admin, 'team', 'get', team.id)['allow_private_access']
    suffix = uuid.uuid4().hex[:8]
    sep = '\\' if member.os == 'windows' else '/'
    base = member.home + sep + 'rx19t-' + suffix
    origin_stop = owner.home + ('\\' if owner.os == 'windows' else '/') + 'rx19t-origin-' + suffix
    port = str(24000 + int(suffix, 16) % 15000)
    with concurrent.futures.ThreadPoolExecutor(2) as pool:
        origin = None
        try:
            team.share(oid, 'exec,private_access')
            team.setting(True)
            time.sleep(3)
            for mutation in ('grant_revoke', 'setting_off'):
                team.setting(True)
                team.share(oid, 'exec,private_access')
                deadline = time.monotonic() + 30
                while True:
                    visible = [m for m in fleet.json_pb(member, '--workspace', team.id,
                               'machine', 'list')['machines'] if m['id'] == oid]
                    if visible and {'exec', 'private_access'} <= set(visible[0]['permissions']): break
                    assert time.monotonic() < deadline, 'allowed target not visible with exec/private_access: %r' % visible
                    time.sleep(1)
                # Server grants arrive before the member daemon's peer snapshot (F9 uses
                # eventually for the same readiness). This check measures revocation, not enrollment.
                deadline = time.monotonic() + 60
                while member.pb('resolve', oid, '--json', timeout=20).rc != 0:
                    assert time.monotonic() < deadline, 'member daemon never learned the granted peer'
                    time.sleep(1)
                if origin is None:
                    origin = pool.submit(origin_controller.pb, 'exec', owner.alias, '--',
                        xplat_access.py(owner), '-c', ORIGIN, origin_stop, port, timeout=540)
                    time.sleep(3)
                state, stop = base + '-' + mutation, base + '-' + mutation + '.stop'
                holder = pool.submit(member.run, [xplat_access.py(member), '-c', HOLDER,
                                     member.pb_path, owner.alias, team.id, port, state, stop], timeout=240)
                try:
                    wait_state(member, state, lambda s: s.get('alive') and s.get('echoes', 0) >= 2, 75)
                    started = time.monotonic()  # before mutation: conservative bound, no cross-device clock skew
                    if mutation == 'grant_revoke': team.share(oid, 'exec')
                    else: team.setting(False)
                    ended = wait_state(member, state, lambda s: s.get('ended'), max(0, 30 - (time.monotonic() - started)))
                    seconds = closure_seconds(ended, started, time.monotonic())
                    done = holder.result(timeout=15)
                    assert done.rc == 0, 'holder failed: ' + done.errtext[-200:]
                    refused = member.pb('access', 'machine', '--workspace', team.id, owner.alias,
                                        '--port', port, '--listen', '127.0.0.1:0', '--json', timeout=45)
                    message = refused.text + refused.errtext
                    needle = 'private_access' if mutation == 'grant_revoke' else 'turned off'
                    assert refused.rc != 0 and needle in message, 'wrong refusal: ' + message[-500:]
                    records[mutation] = {'status': 'PASS', 'seconds': seconds, 'echoes': ended['echoes'],
                                         'reason': ended['reason'], 'refusal': message.strip()}
                    print('PASS X-19:%s:%s>%s %.2fs' % (mutation, owner.os, member.os, seconds), flush=True)
                finally:
                    member.write(stop, b'stop')
                    holder.result(timeout=30)
                    for path in (state, state + '.tmp', stop): member.remove(path)
        finally:
            primary = sys.exception()
            if primary: print("FAIL before cleanup: " + str(primary), flush=True)
            cleanup_errors = []
            # Attempt every cleanup even when an earlier mutation/cleanup failed.
            actions = []
            if origin is not None:
                actions.append(lambda: origin_controller.pb('exec', owner.alias, '--',
                    xplat_access.py(owner), '-c',
                    'import pathlib,sys; pathlib.Path(sys.argv[1]).touch()', origin_stop, timeout=60))
            actions.extend([lambda: team.setting(original),
                            lambda: team.revoke(oid, 'exec,private_access'),
                            lambda: team.unshare(oid)])
            for action in actions:
                try: action()
                except Exception as error: cleanup_errors.append(str(error))
            try:
                if origin is not None:
                    done = origin.result(timeout=30)
                    assert done.rc == 0, 'origin did not exit cleanly: ' + done.errtext[-200:]
            except Exception as error: cleanup_errors.append(str(error))
            if cleanup_errors:
                detail = 'cleanup failed: ' + '; '.join(cleanup_errors)
                if primary: primary.add_note(detail)
                else: raise AssertionError(detail)
    return records


def fleets(pairs):
    owner = {'L': fleet.linux(), 'M': fleet.mac('lanex2-m')}
    saved = fleet.L_HOST, dict(os.environ)
    try:
        fleet.L_HOST = os.environ['XPE_MEMBER_L_HOST']
        for key in ('USER', 'UID', 'DOCKER'):
            os.environ['XPLAT_L_' + key] = os.environ['XPE_MEMBER_L_' + key]
        member = {'L': fleet.linux(os.environ.get('XPE_MEMBER_L_NAME', 'rx19t-l'), ct=os.environ['XPE_MEMBER_L_CT'])}
        member['L'].prefix[1:1] = ['-o', 'ConnectTimeout=15']
        member['W'] = fleet.Windows('rx19t-w', target=os.environ['XPE_MEMBER_WIN_SSH'])
    finally:
        fleet.L_HOST = saved[0]
        os.environ.clear(); os.environ.update(saved[1])
    return owner, member


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--pairs', default='L>M,M>L,L>W,W>L,M>W,W>M')
    args = parser.parse_args()
    pairs = args.pairs.split(',')
    owner, member = fleets(pairs)
    # Invert Mac-client pairs as X-21 does: Y owns L/W, F administers the team.
    member['M'] = owner['M']
    mac = member['M']
    mac.run = lambda argv, input=None, env=None, timeout=120, cwd=None: owner['L'].pb(
        'exec', mac.alias, '--', *argv, input=input, timeout=timeout)
    results = {}
    stamp = time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())
    out = Path(__file__).parent / 'results' / ('xplat-team-private-end-' + stamp + '.json')
    needed = {('owner', 'L'), ('member', 'L'), ('member', 'W')}
    for pair in pairs:
        a, b = pair.split('>')
        needed.update({('member' if b == 'M' or a == 'W' else 'owner', a),
                       ('owner' if b == 'M' or a == 'W' else 'member', b)})
    versions = {}
    for role, key in sorted(needed):
        d = (owner if role == 'owner' else member)[key]
        version = d.pb('version')
        assert version.rc == 0, d.name + ' version unavailable: ' + version.errtext[-200:]
        versions[role + ':' + key] = {'machine': d.name, 'version': version.text.strip()}
    try:
        for pair in pairs:
            a, b = pair.split('>')
            try:
                if b == 'M' or a == 'W':
                    pair_team = xplat_team.Team(owner['L'], owner['L'], member['W'], owner['L'])
                    target, client = member[a], owner[b]
                else:
                    target, client = owner[a], member[b]
                    pair_team = xplat_team.Team(owner['L'], owner['L'], owner['L'], member['W'])
                results[pair] = {}
                matches = [m for m in fleet.json_pb(pair_team.owner, 'machine', 'list',
                               '--q', target.alias)['machines']
                               if m['alias'] == target.alias and m['online']]
                assert len(matches) == 1, 'need one online target %s, found %d' % (target.alias, len(matches))
                oid = matches[0]['id']
                if b == 'M' or a == 'W':
                    origin_controller = member['L'] if a == 'W' else member['W']
                else:
                    origin_controller = owner['M'] if a == 'L' else owner['L']
                run_pair(pair_team, target, client, oid, results[pair], origin_controller)
            except Exception as error:
                results.setdefault(pair, {}).update(status='FAIL', error=str(error))
                raise  # stop on unexplained behaviour; diagnose read-only before any rerun
    finally:
        out.write_text(json.dumps({'check': 'X-19-team-private-end', 'versions': versions, 'results': results}, indent=2))
        print(out, flush=True)


if __name__ == '__main__': main()
