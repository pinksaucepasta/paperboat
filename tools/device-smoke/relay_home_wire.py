#!/usr/bin/env python3
"""F21-161 wire check: the relay a machine advertises as its home is the one its peers use, and it moves when that relay dies.

  relay_home_wire.py PEER --relay-ip NAME=ADDRESS --relay-ip NAME=ADDRESS    (a paired Linux or macOS machine; sudo -n iptables or pfctl;
  with PB_SMOKE_WINDOWS_PB the Windows device, blocked with a firewall rule on its pb.exe)
  NAME is a relay's name in `pb relay list`, ADDRESS the public IPv4 of its host (two relays are needed). Relays on one host
  (a staging stack) are told apart by TCP port: ADDRESS may be IP:PORT, which cuts only that port (Linux).
  1. UDP is dropped (path_display.udp_blocked) so PEER is reached through a relay; `pb status --json` names the home relay
     (home_relay.region, the region of one of the relays of `pb relay list`) once a pair is bound;
  2. the relay that is NOT home is blocked (TCP and UDP to its address): the home relay is unchanged and PEER still answers;
  3. that block is lifted and the HOME relay is blocked: within 90 s the home relay is the other one, and PEER answers over it;
  4. every rule is removed on every exit path.
Users see regions, never relay node ids, so the check compares REGIONS: the status home region against the region `pb relay list`
gives each NAME. This is weaker than comparing relay ids: the two relays must be in different regions, because two relays of one
region cannot be told apart (the script fails saying so).
Prints one JSON line {"check": "relay-home-wire", ...}; exit 1 on failure.
"""
import contextlib
import json
import re
import subprocess
import sys
import time

import path_display as pd
import winhost


def sudo(*args, **kw):
    return subprocess.run(['sudo', '-n', *args], capture_output=True, text=True, timeout=30, **kw)


@contextlib.contextmanager
def blocked(address):
    """Silently drop packets between this machine and ADDRESS (the relay's host) while the context lasts. A Windows device: a firewall
    block of its pb.exe to ADDRESS (a timer on the device lifts it whatever happens here)."""
    device = winhost.device()
    if device:
        cut = device.cut_network(900, addresses=[address])
        try:
            yield
        finally:
            cut.release()
        return
    if pd.DARWIN:
        anchor = 'com.apple/pbsmoke-relay'
        sudo('pfctl', '-a', anchor, '-f', '-', input='block drop out quick to %s\n' % address)
        token = re.search(r'Token : (\d+)', sudo('pfctl', '-E').stderr)
        sudo('pfctl', '-F', 'states')
        try:
            yield
        finally:
            sudo('pfctl', '-a', anchor, '-F', 'all')
            if token:
                sudo('pfctl', '-X', token.group(1))
        return
    ip, _, port = address.partition(':')
    out, inn = ['-d', ip], ['-s', ip]
    if port:
        out += ['-p', 'tcp', '--dport', port]
        inn += ['-p', 'tcp', '--sport', port]
    with winhost.linux_silent_cut(out, inn):  # an OUTPUT DROP would fail sends with EPERM
        yield


def status():
    value = json.loads(pd.pb('status', '--json')[1])
    return value.get('data', value)


def home(peer):
    pd.pb('ping', peer)
    value = status()
    return (value.get('home_relay') or {}).get('region')


def answers(peer, seconds=60):
    deadline = time.time() + seconds
    while time.time() < deadline:
        code, out, _ = pd.pb('exec', peer, '--', 'echo', 'ok')
        if code == 0 and out.strip() == 'ok':
            return True
        time.sleep(3)
    return False


def relay_regions(listed, ips):
    """{region: relay name} for the relays that have an address; the regions must be distinct."""
    regions = {r['name']: r.get('region', '') for r in listed}
    if len(ips) < 2 or not set(ips) <= set(regions):
        raise RuntimeError('two relays of `pb relay list` (%s) need an address each' % ', '.join(sorted(regions)))
    by_region = {regions[name]: name for name in ips}
    if '' in by_region or len(by_region) != len(ips):
        raise RuntimeError('the relays must be in different regions to be told apart by region: %s' % {n: regions[n] for n in ips})
    return by_region


def main():
    argv = sys.argv[1:]
    peer = argv[0]
    ips = dict(item.split('=', 1) for flag, item in zip(argv, argv[1:]) if flag == '--relay-ip')
    listed = json.loads(pd.pb('relay', 'list', '--json')[1])
    listed = listed.get('data', listed)['relays']
    by_region = relay_regions(listed, ips)
    with pd.udp_blocked():
        first = None
        for _ in range(30):
            first = home(peer)
            if first:
                break
            time.sleep(3)
        if first not in by_region:
            raise RuntimeError('status has no home region among %s: %r' % (sorted(by_region), first))
        other = next(i for i in by_region if i != first)
        with blocked(ips[by_region[other]]):
            time.sleep(10)
            after = home(peer)
            if after != first:
                raise RuntimeError('blocking the relay that is not home moved the home relay %s -> %s' % (by_region[first], by_region.get(after)))
            if not answers(peer):
                raise RuntimeError('%s did not answer with the non-home relay blocked' % peer)
        moved_to, began = None, time.time()
        with blocked(ips[by_region[first]]):
            deadline = began + 90
            while time.time() < deadline:
                pd.pb('ping', peer)
                moved_to = home(peer)
                if moved_to == other:
                    break
                time.sleep(3)
            if moved_to != other:
                raise RuntimeError('the home relay %s was blocked and the home relay is still %s after 90 s' % (by_region[first], by_region.get(moved_to)))
            took = time.time() - began
            if not answers(peer):
                raise RuntimeError('%s did not answer over the new home relay %s' % (peer, by_region[other]))
    print(json.dumps({'check': 'relay-home-wire', 'status': 'PASS', 'home_before': by_region[first], 'home_after_block': by_region[other],
                      'failover_seconds': round(took, 1), 'unchanged_when_other_blocked': True}))


if __name__ == '__main__':
    try:
        main()
    except Exception as err:  # noqa: BLE001
        print(json.dumps({'check': 'relay-home-wire', 'status': 'FAIL', 'detail': str(err)}))
        sys.exit(1)
