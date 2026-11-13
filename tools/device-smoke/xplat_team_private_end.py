#!/usr/bin/env python3
"""X-19: held private TCP flows end on grant revoke and team setting off.

Pairs use holder OS > shared target OS. XPLAT_L_HOST supplies two isolated
Linux containers rx19d-a/rx19d-b in distinct accounts. XPLAT_MAC_SSH is the
account-A Mac; XPE_MEMBER_WIN_SSH is the account-B Windows runner. XPE_TEAM
names a throwaway paid team owned by A with B as a member. Direct SSH starts
bounded origins, so grant changes cannot stop the origin itself. Each mutation
holds both a pb access forward and raw 127.100.x.y TCP; both must echo first,
close within 30 seconds, and refuse a fresh exchange.
"""
import argparse
import concurrent.futures
import json
import math
import os
import sys
import subprocess
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
child = subprocess.Popen([pb,'access','machine','--workspace',team,alias,
    '--port',port,'--listen','127.0.0.1:0','--json'],stdout=subprocess.PIPE,stderr=subprocess.PIPE)
lines=[]
threading.Thread(target=lambda:lines.append(child.stdout.readline()),daemon=True).start()
def publish(**value):
    with open(state+'.tmp','w') as f: json.dump(value,f)
    os.replace(state+'.tmp',state)
try:
    end=time.monotonic()+60
    while not lines and time.monotonic()<end and child.poll() is None: time.sleep(.1)
    receipt=json.loads(lines[0]) if lines else {}
    data=receipt.get('data',receipt)
    assert data.get('listen_address'), 'access did not publish listener: '+str(receipt)
    host,local=data['listen_address'].rsplit(':',1)
    resolved=subprocess.run([pb,'--workspace',team,'resolve',alias,'--json'],capture_output=True,text=True,timeout=20)
    address=json.loads(resolved.stdout); address=address.get('data',address)
    address=address.get('assigned_ip') or address.get('address')
    assert address and address.startswith('127.100.'), 'no private address'
    sockets={'access':socket.create_connection((host,int(local)),timeout=10),
             'raw':socket.create_connection((address,int(port)),timeout=10)}
    counts={k:0 for k in sockets}; ended={}; end=time.monotonic()+180
    for c in sockets.values(): c.settimeout(2)
    while not os.path.exists(stop) and time.monotonic()<end:
        for kind,c in list(sockets.items()):
            try:
                c.sendall(b'rx19d-echo'); got=b''
                while len(got)<10:
                    b=c.recv(10-len(got))
                    if not b: break
                    got+=b
                if got!=b'rx19d-echo':
                    ended[kind]={'reason':'EOF','echoes':counts[kind]}; c.close(); del sockets[kind]
                else: counts[kind]+=1
            except socket.timeout: pass
            except OSError as error:
                ended[kind]={'reason':type(error).__name__,'echoes':counts[kind]}; c.close(); del sockets[kind]
        publish(alive=not ended,ended=len(ended)==2,echoes=min(counts.values()),flows=ended,address=address)
        if not sockets: break
        time.sleep(.25)
    else: publish(error='holder stopped without both flow closures',echoes=min(counts.values()))
    for c in sockets.values(): c.close()
except Exception as error: publish(error=type(error).__name__+': '+str(error))
finally:
    if child.poll() is None: child.terminate()
    try: child.wait(5)
    except subprocess.TimeoutExpired: child.kill(); child.wait()
'''


RAW_PROBE = r'''
import json, socket, sys
phase='connect'
try:
    s=socket.create_connection((sys.argv[1],int(sys.argv[2])),timeout=5)
    phase='receive'
    s.settimeout(5); s.sendall(b'denied'); b=s.recv(6)
    result={'phase':phase,'outcome':'EOF' if not b else 'data'}
    s.close()
except socket.timeout: result={'phase':phase,'outcome':'timeout'}
except (ConnectionRefusedError,ConnectionResetError,BrokenPipeError) as error:
    result={'phase':phase,'outcome':type(error).__name__}
print(json.dumps(result))
'''

def raw_denied(os_name, probe):
    if probe.get('outcome') in ('EOF', 'ConnectionRefusedError', 'ConnectionResetError', 'BrokenPipeError'):
        return True
    # macOS PF deliberately drops unleased SYNs; a connected but silent socket is never proof.
    return os_name == 'darwin' and probe == {'phase': 'connect', 'outcome': 'timeout'}


def closure_seconds(state, started, observed):
    """Only a previously flowing connection's observed EOF/reset proves closure."""
    elapsed = observed - started
    assert state.get('ended') and state.get('echoes', 0) >= 2, 'no held-flow closure: %r' % state
    assert set(state.get('flows', {})) == {'access', 'raw'}, 'both transports must close'
    assert all(f.get('echoes', 0) >= 2 for f in state['flows'].values()), 'both transports must flow'
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


def start_origin(controller, owner, stop, port):
    argv = [xplat_access.py(owner), '-c', ORIGIN, stop, port]
    if owner.os == 'windows':
        # Same detached CIM lifecycle as xplat_team.origin_start / winhost.start.
        command = subprocess.list2cmdline(argv).replace("'", "''")
        script = ("$r=Invoke-CimMethod -ClassName Win32_Process -MethodName Create "
                  "-Arguments @{CommandLine='%s'}; "
                  "if($r.ReturnValue -ne 0){throw 'origin CIM launch failed'}; $r.ProcessId") % command
        argv = ['powershell.exe', '-NoProfile', '-NonInteractive', '-Command', script]
    return owner.run(argv, timeout=540)


def stop_origin(controller, owner, stop):
    code = ('import pathlib,sys,time; p=pathlib.Path(sys.argv[1]); p.touch(); '
            'end=time.monotonic()+15\n'
            'while p.exists() and time.monotonic()<end: time.sleep(.2)\n'
            'assert not p.exists(), "origin did not stop"')
    done = owner.run([xplat_access.py(owner), '-c', code, stop], timeout=60)
    assert done.rc == 0, 'origin cleanup: ' + (done.text + done.errtext)[-400:]


def run_pair(team, owner, member, oid, records, origin_controller):
    original = fleet.json_pb(team.admin, 'team', 'get', team.id)['allow_private_access']
    suffix = uuid.uuid4().hex[:8]
    sep = '\\' if member.os == 'windows' else '/'
    base = member.home + sep + 'rx19d-' + suffix
    origin_stop = owner.home + ('\\' if owner.os == 'windows' else '/') + 'rx19d-origin-' + suffix
    port = str(24000 + int(suffix, 16) % 15000)
    records['fixture'] = {'team': team.id, 'owner': owner.alias, 'member': member.alias,
                          'machine_id': oid, 'origin_port': port, 'granted': ['exec', 'private_access']}
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
                    origin = pool.submit(start_origin, origin_controller, owner, origin_stop, port)
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
                    records[mutation] = {'status': 'CLOSURE_ONLY', 'seconds': seconds, 'echoes': ended['echoes'],
                                         'flows': ended['flows'], 'address': ended['address'], 'refusal': message.strip()}
                    raw = member.run([xplat_access.py(member), '-c', RAW_PROBE,
                                      ended['address'], port], timeout=15)
                    assert raw.rc == 0, 'raw probe failed: ' + raw.errtext[-200:]
                    probe = json.loads(raw.text)
                    records[mutation]['raw_refusal'] = probe
                    assert raw_denied(member.os, probe), 'raw TCP fresh attempt: %r' % probe
                    records[mutation]['status'] = 'PASS'
                    print('PASS X-19:%s:%s>%s %.2fs' % (mutation, member.os, owner.os, seconds), flush=True)
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
                actions.append(lambda: stop_origin(origin_controller, owner, origin_stop))
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


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--pairs', default='L>M,M>L,L>W,W>L,M>W,W>M')
    args = parser.parse_args()
    # Two accounts, each Linux controller stays outside the team transport.
    a = fleet.linux('rx19d-a', ct='rx19d-a')
    b = fleet.linux('rx19d-b', ct='rx19d-b')
    m = fleet.mac('rx19d-m')
    w = fleet.Windows('rx19d-w', target=os.environ['XPE_MEMBER_WIN_SSH'])
    combinations = {'L>M': (b,m), 'M>L': (m,b), 'L>W': (a,w),
                    'W>L': (w,a), 'M>W': (m,w), 'W>M': (w,m)}
    results, versions = {}, {}
    stamp = time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())
    out = Path(__file__).parent / 'results' / ('xplat-manual-X-19-team-' + stamp + '.json')
    for d in (a,b,m,w):
        version = d.pb('version')
        assert version.rc == 0, d.name + ' version unavailable'
        assert 'dev-ac409910b' in version.text, 'wrong cycle build: ' + d.name
        versions[d.name] = version.text.strip()
    try:
        for pair in args.pairs.split(','):
            client, target = combinations[pair]
            target_ctl = a if target in (a,m) else b
            team = xplat_team.Team(a,a,target_ctl,client)
            results[pair] = {}
            try:
                matches = [x for x in fleet.json_pb(target_ctl,'machine','list')['machines']
                           if x['alias'] == target.alias and x['online']]
                assert len(matches) == 1, 'target not online: ' + target.alias
                run_pair(team,target,client,matches[0]['id'],results[pair],target)
            except Exception as error:
                results[pair].update(status='FAIL',error=str(error))
                print('FAIL X-19:' + pair + ': ' + str(error), flush=True)
    finally:
        out.write_text(json.dumps({'check':'X-19-team-private-end',
            'direction':'holder>shared target', 'versions':versions,'results':results},indent=2))
        print(out,flush=True)


if __name__ == '__main__': main()
