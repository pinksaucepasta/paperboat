#!/usr/bin/env python3
"""F5-12 offline half on a fleet host: an SSH session started while Paperboat is unreachable is audited with origin 'offline'.

  ssh_offline_audit_fleet.py [--target M] [--client L]      (xplat_fleet devices: Unix target, Linux client; env as for xplat_*.py)
ssh_offline_audit.sh does this on two fresh Linux containers; this one runs on a paired host of any Unix OS (the macOS host of F5-12 M):
the client runs one `pb ssh TARGET` while every Paperboat address is dropped on BOTH sides (pf anchor on macOS, iptables on Linux; the
addresses are the API host and every server-owned host of live discovery, resolved on the target and the client), for 150 s (past the
signed network's 2 minute refresh hint, so the host records origin 'offline'). The block is verified from the target (no connect), the
rules are removed on every exit path, and the audit rows of the target (production database, read-only) must hold an offline start and
its end: same session id, outcome closed, bytes in and out above zero. Prints one JSON line.
"""
import ipaddress
import json
import os
import re
import subprocess
import sys
import time
from urllib.parse import urlparse

import xplat_fleet as fleet

DB_HOST = os.environ.get('PB_AUDIT_DB_HOST', 'root@coolify')
OUTAGE_SECONDS = 150
ANCHOR = 'com.apple/pbsmoke-offline'
RESOLVE = ("import socket,sys\n"
           "for h in sys.argv[1:]:\n"
           "    try:\n"
           "        for r in socket.getaddrinfo(h, 443, proto=socket.IPPROTO_TCP): print(h + '|' + r[4][0])\n"
           "    except OSError: pass\n")


def discovery_hosts(target):
    shown = json.loads(target.pb('config', 'show', '--json').text)
    api = urlparse(shown.get('data', shown)['server_url']).hostname
    body = target.sh('curl -fsS -m 10 https://%s/v1/client-configuration' % api).text
    data = json.loads(body).get('data', {})
    found = {api.lower().rstrip('.')}
    for key in ('issuer', 'installer_base_url', 'console_url', 'cli_verification_url', 'machines_url'):
        host = urlparse(data.get(key) or '').hostname
        if host:
            found.add(host.lower().rstrip('.'))
    for region in data.get('control_regions') or []:
        host = urlparse((region or {}).get('api_origin') or '').hostname
        if host:
            found.add(host.lower().rstrip('.'))
    for key in ('preview_base_domain', 'tunnel_base_domain', 'runtime_base_domain'):
        value = data.get(key)
        if isinstance(value, str) and value and '/' not in value and ' ' not in value:
            found.add(value.lower().rstrip('.'))
    return sorted(found)


def resolved(dev, hosts):
    out = dev.run(['python3', '-c', RESOLVE, *hosts]).text
    return [line.split('|', 1) for line in out.splitlines() if '|' in line]


def block(dev, addresses):
    if dev.os == 'darwin':
        rules = ''.join('block drop out quick to %s\n' % a for a in addresses)
        dev.sh('sudo -n pfctl -a %s -f - ; sudo -n pfctl -E >/dev/null 2>&1; sudo -n pfctl -F states >/dev/null 2>&1' % ANCHOR, input=rules.encode())
    else:
        for a in addresses:
            dev.sh('sudo -n ip %s route add blackhole %s' % ('-6' if ':' in a else '', a))


def unblock(dev, addresses):
    if dev.os == 'darwin':
        dev.sh('sudo -n pfctl -a %s -F all' % ANCHOR)
    else:
        for a in addresses:
            dev.sh('sudo -n ip %s route del blackhole %s >/dev/null 2>&1; true' % ('-6' if ':' in a else '', a))


def audit_rows(target_id):
    query = ("SELECT event_type, metadata->>'origin', metadata->>'session_id', metadata->>'outcome', coalesce(metadata->>'bytes_in','0'), "
             "coalesce(metadata->>'bytes_out','0'), metadata->>'peer_endpoint_id', idempotency_key FROM paperboat.audit_events "
             "WHERE resource_id='%s' AND event_type IN ('machine.access_started','machine.access_ended') AND metadata->>'capability'='managed_ssh' "
             "ORDER BY created_at, id;" % target_id)
    done = subprocess.run(['ssh', DB_HOST, 'docker', 'exec', '-i', 'paperboat-postgres-1', 'psql', '-U', 'postgres', '-d', 'paperboat', '-qAt', '-F', '|'],
                          input=query, capture_output=True, text=True, timeout=120)
    return [row.split('|') for row in done.stdout.splitlines() if row]


def main():
    argv = sys.argv[1:]
    devices = fleet.fleet()
    target = devices[argv[argv.index('--target') + 1] if '--target' in argv else 'M']
    client = devices[argv[argv.index('--client') + 1] if '--client' in argv else 'L']
    stamp = str(int(time.time()))
    target_id = fleet.machines()[target.alias]['id']
    first = client.pb('ssh', target.alias, '--', 'echo', 'baseline-' + stamp, timeout=180)
    assert 'baseline-' + stamp in first.text, 'pb ssh does not run before the outage: %s' % (first.errtext or first.text)[-200:]
    hosts = discovery_hosts(target)
    pairs = resolved(target, hosts)
    addresses = sorted({ip for _, ip in pairs} | {ip for _, ip in resolved(client, hosts)}, key=lambda a: (ipaddress.ip_address(a).version, a))
    assert pairs and addresses, 'no Paperboat address resolved'
    blocked = False
    try:
        block(target, addresses)
        block(client, addresses)
        blocked = True
        # the block must hold before the outage window: no hostname/address pair the target resolves may connect
        bad = 0
        for host, ip in pairs:
            if target.run(['curl', '-s', '-o', '/dev/null', '-m', '6', '--connect-timeout', '5', '-k', '--resolve', '%s:443:%s' % (host, ip), 'https://%s/' % host]).rc == 0:
                bad += 1
        assert bad == 0, 'the block leaks: %d of %d addresses still connect' % (bad, len(pairs))
        began = time.time()
        time.sleep(OUTAGE_SECONDS)
        offline = client.pb('ssh', target.alias, '--', 'echo', 'offline-' + stamp, timeout=120)
        assert 'offline-' + stamp in offline.text, 'pb ssh did not run during the outage: %s' % (offline.errtext or offline.text)[-200:]
    finally:
        if blocked:
            unblock(target, addresses)
            unblock(client, addresses)
    outage = int(time.time() - began)
    rows = []
    for _ in range(30):
        rows = audit_rows(target_id)
        if any(r[1] == 'offline' for r in rows):
            break
        time.sleep(5)
    starts = [r for r in rows if r[0] == 'machine.access_started' and r[1] == 'offline']
    ends = [r for r in rows if r[0] == 'machine.access_ended' and r[1] == 'offline']
    start_ids = {s[7].removeprefix('offline:') for s in starts}
    ok = bool(starts) and bool(ends) and all(s[6].startswith('session_') for s in starts)
    ok = ok and all(e[3] == 'closed' and int(e[4]) > 0 and int(e[5]) > 0 and e[2] in start_ids for e in ends)
    print(json.dumps({'check': 'ssh-offline-audit-fleet', 'status': 'PASS' if ok else 'FAIL', 'target': target.os, 'client': client.os,
                      'outage_seconds': outage, 'addresses_blocked': len(addresses), 'offline_starts': len(starts), 'offline_ends': len(ends),
                      'end_outcome': ends[0][3] if ends else None, 'end_bytes': [ends[0][4], ends[0][5]] if ends else None}))
    sys.exit(0 if ok else 1)


if __name__ == '__main__':
    main()
