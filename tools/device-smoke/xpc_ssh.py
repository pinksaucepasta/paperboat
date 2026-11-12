#!/usr/bin/env python3
"""X-29: forwarding through `pb ssh` between each pair of OSes: -L, -R, -D, agent forwarding, no X11.

  xpc_ssh.py [--devices L,M,W] [--only forward,agent,x11]

forward: for every ordered pair (client A, target B) one Python driver runs ON A. -L: a digest origin on B (started with `pb exec`)
is reached through `pb ssh B -- -L`; -D: the same origin through a SOCKS5 proxy opened with `pb ssh B -- -D`; -R: a digest origin
on A is reached from B through `pb ssh B -- -R` (B's own Python is the client). Each exchange sends 1 MiB, half-closes and compares
the digest the origin computed.
agent: `pb ssh -A` into L and M gives the target a working agent (`ssh-add -l` lists pb's own key; the client's own agent is not what
is forwarded, ssh_forwarding.sh); into a Windows target pb says
that an agent cannot be forwarded (the documented launch limitation) and the key is not offered there.
x11: no DISPLAY on the target (pb never requests X11).
Results: results/xpc-ssh-<stamp>.json (`X-29:forward:L>W`).
"""
import json
import sys
import time
from itertools import permutations
from pathlib import Path

HERE = Path(__file__).parent
sys.path.insert(0, str(HERE))
import xplat_access  # noqa: E402
import xplat_fleet as fleet  # noqa: E402

results = {}

DRIVER = r'''
import hashlib, json, socket, struct, subprocess, sys, time
pb, machine, op, lp, rp, dp, cp, origin_code, bpy = sys.argv[1:10]
PAYLOAD = (("forward λ船\n".encode() + bytes(range(256))) * 4096)[:1 << 20]
want = "%d %s" % (len(PAYLOAD), hashlib.sha256(PAYLOAD).hexdigest())
quiet = dict(stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
def listen(port, proc):
    end = time.time() + 40
    while time.time() < end:
        try:
            socket.create_connection(("127.0.0.1", int(port)), timeout=2).close(); return
        except OSError:
            time.sleep(1)
    if proc.poll() is None: proc.terminate()
    raise RuntimeError("nothing listens on %s; pb ssh said: %s" % (port, (proc.stderr.read() + proc.stdout.read()).decode(errors="replace")[-400:]))
def exchange(sock):
    sock.sendall(PAYLOAD); sock.shutdown(socket.SHUT_WR)
    got = b""
    while True:
        b = sock.recv(4096)
        if not b: break
        got += b
    sock.close(); return got.decode().strip()
def socks(port, target):
    s = socket.create_connection(("127.0.0.1", int(port)), timeout=30)
    s.sendall(b"\x05\x01\x00"); assert s.recv(2) == b"\x05\x00", "no SOCKS5 answer"
    s.sendall(b"\x05\x01\x00\x01" + socket.inet_aton("127.0.0.1") + struct.pack("!H", int(target)))
    reply = s.recv(10); assert reply[:2] == b"\x05\x00", "SOCKS connect refused: %r" % reply
    return s
def stop(p):
    if p.poll() is None:
        p.terminate()
        try: p.wait(10)
        except Exception: p.kill()
out = {}
procs = []
try:
    origin = subprocess.Popen([pb, "exec", machine, "--", bpy, "-c", origin_code, op], **quiet); procs.append(origin)
    local_origin = subprocess.Popen([sys.executable, "-c", origin_code, cp], **quiet); procs.append(local_origin)
    time.sleep(6)
    fwd = subprocess.Popen([pb, "ssh", machine, "--", "-N", "-o", "ExitOnForwardFailure=yes", "-L", "127.0.0.1:%s:127.0.0.1:%s" % (lp, op)], **quiet); procs.append(fwd)
    listen(lp, fwd); out["L"] = exchange(socket.create_connection(("127.0.0.1", int(lp)), timeout=30)) == want; stop(fwd)
    fwd = subprocess.Popen([pb, "ssh", machine, "--", "-N", "-o", "ExitOnForwardFailure=yes", "-D", "127.0.0.1:%s" % dp], **quiet); procs.append(fwd)
    listen(dp, fwd); out["D"] = exchange(socks(dp, op)) == want; stop(fwd)
    fwd = subprocess.Popen([pb, "ssh", machine, "--", "-N", "-o", "ExitOnForwardFailure=yes", "-R", "127.0.0.1:%s:127.0.0.1:%s" % (rp, cp)], **quiet); procs.append(fwd)
    time.sleep(10)
    client = ("import hashlib,socket,sys\nP=((\"forward \\u03bb\\u8239\\n\".encode()+bytes(range(256)))*4096)[:1<<20]\n"
              "s=socket.create_connection(('127.0.0.1',int(sys.argv[1])),timeout=30)\ns.sendall(P);s.shutdown(socket.SHUT_WR)\n"
              "g=b''\nwhile True:\n b=s.recv(4096)\n if not b: break\n g+=b\nprint(g.decode().strip())\n")
    done = subprocess.run([pb, "exec", machine, "--", bpy, "-c", client, rp], capture_output=True, text=True, timeout=90)
    out["R"] = done.stdout.strip() == want
    out["R_detail"] = (done.stdout + done.stderr).strip()[-120:]
finally:
    for p in procs: stop(p)
print(json.dumps(out))
'''


def py(dev):
    return 'python' if dev.os == 'windows' else 'python3'


def t_forward(src, dst):
    base = 24000 + sum(map(ord, src.name + dst.name)) % 1500
    ports = [str(base + 100 * i) for i in range(5)]  # origin, -L, -R, -D, local origin
    done = src.run([py(src), '-c', DRIVER, src.pb_path, dst.alias, ports[0], ports[1], ports[2], ports[3], ports[4], xplat_access.ORIGIN, py(dst)], timeout=360)
    assert done.rc == 0, 'driver: %s %s' % (done.text[-300:], done.errtext[-300:])
    out = json.loads(done.text.strip().splitlines()[-1])
    assert out.get('L') and out.get('D') and out.get('R'), 'forwards not byte-exact: %s' % out
    return '-L, -D (SOCKS5) and -R each carried 1 MiB byte-exact with half-close'


AGENT_UNIX = r'''
set -e
d=$(mktemp -d); trap 'ssh-agent -k >/dev/null 2>&1 || true; rm -rf "$d"' EXIT
eval "$(ssh-agent -s)" >/dev/null
ssh-keygen -q -t ed25519 -N '' -C xpc-agent-key -f "$d/k"
ssh-add -q "$d/k"
want=$(ssh-keygen -lf "$d/k.pub" | cut -d' ' -f2)
echo "WANT $want"
"$1" ssh "$2" -- -A 'ssh-add -l' 2>&1 || true
'''

AGENT_WINDOWS = r'''
$ErrorActionPreference = 'Continue'
$pb = $args[0]
Set-Service ssh-agent -StartupType Manual -ErrorAction SilentlyContinue
Start-Service ssh-agent -ErrorAction SilentlyContinue
$d = Join-Path $env:TEMP ('xpc-agent-' + [guid]::NewGuid().ToString('N')); New-Item -ItemType Directory $d | Out-Null
ssh-keygen -q -t ed25519 -N '""' -C xpc-agent-key -f "$d\k"
ssh-add "$d\k" 2>&1 | Out-Null
$want = (ssh-keygen -lf "$d\k.pub").Split(' ')[1]
"WANT $want"
& $pb ssh $args[1] -- -A 'ssh-add -l' 2>&1
ssh-add -d "$d\k" 2>&1 | Out-Null
Remove-Item -Recurse -Force $d
'''


def t_agent(src, dst):
    if src.os == 'windows':
        done = src.sh(AGENT_WINDOWS.replace('$args[0]', "'%s'" % src.pb_path).replace('$args[1]', "'%s'" % dst.alias), timeout=240)
    else:
        done = src.run(['bash', '-c', AGENT_UNIX, 'x', src.pb_path, dst.alias], timeout=240)
    text = (done.text + done.errtext)
    want = next((line.split()[1] for line in text.splitlines() if line.startswith('WANT ')), None)
    assert want, 'no agent key on the client: ' + text[-300:]
    text = '\n'.join(line for line in text.splitlines() if not line.startswith('WANT '))
    if dst.os == 'windows':
        assert want not in text and 'agent' in text.lower(), 'a Windows target must say an agent cannot be forwarded and must not see the key: ' + text[-400:]
        notice = next((line for line in text.splitlines() if 'agent' in line.lower()), '')
        return 'into Windows the key is not offered and pb says: ' + notice.strip()[:140]
    # `pb ssh -A` forwards pb's own agent (the user's IdentityAgent for the managed key, see ssh_forwarding.sh), not the client's agent
    keys = [line for line in text.splitlines() if 'ED25519' in line or 'RSA' in line]
    assert keys and 'Could not open a connection' not in text, 'no forwarded agent on the target: ' + text[-300:]
    return 'ssh-add -l on the target lists the forwarded agent key: %s' % keys[0].strip()[:90]


def t_x11(src, dst):
    done = src.pb('ssh', dst.alias, '--', 'echo "DISPLAY=[$DISPLAY]"' if dst.os != 'windows' else 'echo DISPLAY=[%DISPLAY%]', timeout=120)
    assert done.rc == 0 and 'DISPLAY=[]' in done.text.replace('%DISPLAY%', ''), 'a DISPLAY reached the target: ' + done.text[-100:]
    return 'no DISPLAY on the target'


TESTS = [('forward', t_forward), ('agent', t_agent), ('x11', t_x11)]


def check(name, pair, action):
    key = 'X-29:%s:%s' % (name, pair)
    started = time.time()
    try:
        detail = action()
        results[key] = {'status': 'PASS', 'detail': detail or ''}
    except Exception as error:  # noqa: BLE001
        results[key] = {'status': 'FAIL', 'detail': str(error)[:700]}
    print('%s %s %ds %s' % (results[key]['status'], key, time.time() - started, results[key]['detail'][:300]), flush=True)


def main():
    argv = sys.argv[1:]
    keys = argv[argv.index('--devices') + 1].split(',') if '--devices' in argv else ['L', 'M', 'W']
    only = set(argv[argv.index('--only') + 1].split(',')) if '--only' in argv else set()
    devices = {k: d for k, d in fleet.fleet().items() if k in keys}
    for a, b in permutations(devices, 2):
        for name, test in TESTS:
            if not only or name in only:
                check(name, '%s>%s' % (a, b), lambda: test(devices[a], devices[b]))
    out = HERE / 'results' / ('xpc-ssh-%s.json' % time.strftime('%Y%m%dT%H%M%SZ', time.gmtime()))
    out.parent.mkdir(exist_ok=True)
    out.write_text(json.dumps({'check': 'xpc-ssh', 'devices': keys, 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
