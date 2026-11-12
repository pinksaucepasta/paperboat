#!/usr/bin/env python3
"""X-22: the daily loop on a mixed fleet (one L, one M, one W machine of ONE account), then a reboot and a network change.

  xpc_daily.py [--devices L,M,W] [--skip-recovery]

For every ordered pair A>B: name resolution (`pb resolve`), `pb exec` (the target's own hostname comes back), ENV (one variable set
once is seen by the target), a file sent into the target's Inbox (sha256), private TCP (xplat_access.t_access). Per machine: the console
inventory lists it online with its OS, a preview of a local origin is reachable, `pb doctor` is healthy. Config sync with one
shared TOML runs through xplat_config.py (X-01). Then the Linux container restarts (a reboot of that machine) and loses and regains its
network (`docker network disconnect/connect`): the other machines reach it again without any user action and doctor is healthy
on all three. A hosted Mac or Windows runner cannot reboot or change networks (README, macpool/winpool), so those two
steps run on L only. Results: results/xpc-daily-<stamp>.json (`X-22:<step>:<A>><B>`).
"""
import hashlib
import json
import re
import subprocess
import sys
import time
import uuid
from itertools import permutations
from pathlib import Path

HERE = Path(__file__).parent
sys.path.insert(0, str(HERE))
import xplat_access  # noqa: E402
import xplat_fleet as fleet  # noqa: E402
import xplat_send  # noqa: E402
from xpc_tunnel import Origin, curl, kill_fixtures, port_for  # noqa: E402

results = {}
VAULT_PASSWORD = Path.home() / '.pbmac/xpc/vault.pw'
VALUE = 'x22 value with $dollar "quotes" üñ \U0001F680'


def record(name, action):
    started = time.time()
    try:
        detail = action()
        results[name] = {'status': 'PASS', 'detail': detail or ''}
    except Exception as error:  # noqa: BLE001
        results[name] = {'status': 'FAIL', 'detail': str(error)[:700]}
    print('%s %s %ds %s' % (results[name]['status'], name, time.time() - started, results[name]['detail'][:300]), flush=True)


def py(dev):
    return 'python' if dev.os == 'windows' else 'python3'


def setup_env(devices):
    """One ENV vault for the account: created on L (the controller), unlocked on the others; one variable set once."""
    if not VAULT_PASSWORD.exists():
        VAULT_PASSWORD.write_text(uuid.uuid4().hex + 'Aa1!')
        VAULT_PASSWORD.chmod(0o600)
    password = VAULT_PASSWORD.read_bytes().strip() + b'\n'
    ctl = fleet.controller()
    status = ctl.pb('env', 'vault', 'unlock', '--password-stdin', input=password, timeout=120)
    if status.rc != 0:
        done = ctl.pb('env', 'vault', 'init', '--password-stdin', input=password, timeout=120)
        assert done.rc == 0, 'vault init: ' + (done.text + done.errtext)[-300:]
    for dev in devices.values():
        done = dev.pb('env', 'vault', 'unlock', '--password-stdin', input=password, timeout=120)
        assert done.rc == 0, '%s vault unlock: %s' % (dev.name, (done.text + done.errtext)[-200:])
    done = ctl.pb('env', 'set', 'X22_VALUE', '--value-stdin', input=VALUE.encode(), timeout=120)
    assert done.rc == 0, 'env set: ' + (done.text + done.errtext)[-300:]


def t_inventory(devices):
    listed = fleet.json_pb(next(iter(devices.values())), 'machine', 'list')['machines']
    seen = {m['alias']: m for m in listed}
    want = {'linux': 'L', 'darwin': 'M', 'windows': 'W'}
    for key, dev in devices.items():
        machine = seen[dev.alias]
        assert machine.get('state') == 'online' or machine.get('online'), '%s not online: %s' % (dev.alias, machine)
        platform = json.dumps(machine).lower()
        assert dev.os in platform or {'darwin': 'macos'}.get(dev.os, dev.os) in platform, '%s: no OS %s in %s' % (dev.alias, dev.os, machine)
    return 'console inventory: %s online with their OS' % ', '.join(d.alias for d in devices.values())


def t_resolve(src, dst):
    data = fleet.json_pb(src, 'resolve', dst.alias)
    assert data.get('assigned_ip') or data.get('address'), data
    return 'resolved %s' % (data.get('assigned_ip') or data.get('address'))


def t_exec(src, dst):
    done = src.pb('exec', dst.alias, '--', 'hostname', timeout=120)
    host = dst.run(['hostname']).text.strip()
    assert done.rc == 0 and done.text.strip() == host, 'pb exec hostname %r, device says %r: %s' % (done.text.strip(), host, done.errtext[-200:])
    return 'hostname %s' % host


def t_env(src, dst):
    code = 'import os,hashlib;print(hashlib.sha256(os.environ.get("X22_VALUE","").encode()).hexdigest())'
    end, seen = time.time() + 120, ''
    while time.time() < end:  # delivery follows the set by a few seconds
        done = src.pb('exec', dst.alias, '--', py(dst), '-c', code, timeout=120)
        seen = done.text.strip()
        if seen == hashlib.sha256(VALUE.encode()).hexdigest():
            return 'X22_VALUE equal (sha256) on %s' % dst.name
        time.sleep(8)
    raise AssertionError('the variable did not reach %s: %r %s' % (dst.name, seen, done.errtext[-150:]))


def t_send(src, dst):
    content = ('x22 file from %s to %s\n' % (src.name, dst.name)).encode() * 100
    before = xplat_send.inbox_listing(dst)
    kind, ok, detail = xplat_send.one_name(src, dst, 'x22-%s.txt' % uuid.uuid4().hex[:6], content, before)
    xplat_send.inbox_clean(dst, before)
    assert kind == 'arrived' and ok, '%s: %s' % (kind, detail)
    return 'file arrived (sha256) in %s Inbox' % dst.name


def t_preview(dev):
    port, marker = port_for(dev, 'preview'), 'x22-preview-' + dev.os
    with Origin(dev, 'http', '127.0.0.1', str(port), marker):
        done = dev.pb('preview', str(port), '--background', '--ttl', '5m', '--json', timeout=180)
        text = done.text
        urls = re.findall(r'https://[A-Za-z0-9._-]+', text)
        ident = re.search(r'"id"\s*:\s*"([^"]+)"', text)
        assert done.rc == 0 and urls and ident, 'preview: ' + (text + done.errtext)[-300:]
        try:
            for _ in range(10):
                out = curl(urls[0] + '/', '--http1.1').stdout.decode().rsplit('\n', 1)
                if out[0] == marker:
                    return 'preview %s serves the marker' % urls[0].split('//')[1][:14]
                time.sleep(4)
            raise AssertionError('preview does not serve the marker: %r' % out[-1])
        finally:
            dev.pb('preview', 'stop', ident.group(1), '--json', timeout=60)
            kill_fixtures(dev)


def t_doctor(dev):
    done = dev.pb('doctor', '--json', timeout=180)
    data = json.loads(done.text)
    data = data.get('data', data)
    bad = [c for c in data.get('checks', []) if c.get('status') not in ('ok', 'pass', 'passed', 'healthy', 'skipped', 'not_applicable')]
    assert done.rc == 0 and not bad, 'doctor: rc %d, not ok: %s' % (done.rc, json.dumps(bad)[:400])
    return 'doctor healthy (%d checks)' % len(data.get('checks', []))


def t_config():
    done = subprocess.run([sys.executable, str(HERE / 'xplat_config.py'), '--only', 'X-01'], capture_output=True, text=True, timeout=1800)
    lines = [line for line in done.stdout.splitlines() if line.startswith(('PASS', 'FAIL'))]
    assert done.returncode == 0, '\n'.join(lines)[-500:]
    return 'config sync with one shared TOML (xplat_config X-01): %d checks PASS' % len([x for x in lines if x.startswith('PASS')])


def wait_online(src, target, seconds=240):
    end = time.time() + seconds
    while time.time() < end:
        listed = fleet.json_pb(src, 'machine', 'list', '--q', target.alias)['machines']
        if any(m['alias'] == target.alias and (m.get('state') == 'online' or m.get('online')) for m in listed):
            done = src.pb('exec', target.alias, '--', 'hostname', timeout=90)
            if done.rc == 0:
                return time.time() - end + seconds
        time.sleep(6)
    raise AssertionError('%s was not reachable again within %ds' % (target.alias, seconds))


def recovery(devices):
    target = devices['L']
    others = [d for k, d in devices.items() if k != 'L']
    subprocess.run(['ssh', fleet.L_HOST, 'docker', 'restart', fleet.L_CT], check=True, capture_output=True, timeout=180)
    for src in others:
        record('X-22:reboot:%s>L' % src.os[0].upper(), lambda: 'reachable again after %.0fs with no user action' % wait_online(src, target, 360))
    subprocess.run(['ssh', fleet.L_HOST, 'docker', 'network', 'disconnect', 'bridge', fleet.L_CT], check=True, capture_output=True, timeout=60)
    time.sleep(20)
    subprocess.run(['ssh', fleet.L_HOST, 'docker', 'network', 'connect', 'bridge', fleet.L_CT], check=True, capture_output=True, timeout=60)
    for src in others:
        record('X-22:network:%s>L' % src.os[0].upper(), lambda: 'reachable again after %.0fs of lost and regained network' % wait_online(src, target, 360))


def main():
    argv = sys.argv[1:]
    keys = argv[argv.index('--devices') + 1].split(',') if '--devices' in argv else ['L', 'M', 'W']
    devices = {k: d for k, d in fleet.fleet().items() if k in keys}
    record('X-22:env-setup', lambda: setup_env(devices) or 'vault unlocked on every machine, X22_VALUE set')
    record('X-22:inventory', lambda: t_inventory(devices))
    for a, b in permutations(devices, 2):
        for name, action in (('resolve', t_resolve), ('exec', t_exec), ('env', t_env), ('send', t_send), ('access', xplat_access.t_access)):
            record('X-22:%s:%s>%s' % (name, a, b), lambda: action(devices[a], devices[b]))
    for key, dev in devices.items():
        record('X-22:preview:' + key, lambda: t_preview(dev))
        record('X-22:doctor:' + key, lambda: t_doctor(dev))
    record('X-22:config', t_config)
    if 'L' in devices and '--skip-recovery' not in argv:
        recovery(devices)
        for key, dev in devices.items():
            record('X-22:doctor-after:' + key, lambda: t_doctor(dev))
    out = HERE / 'results' / ('xpc-daily-%s.json' % time.strftime('%Y%m%dT%H%M%SZ', time.gmtime()))
    out.parent.mkdir(exist_ok=True)
    out.write_text(json.dumps({'check': 'xpc-daily', 'devices': keys, 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
