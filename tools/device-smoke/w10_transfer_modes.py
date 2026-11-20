#!/usr/bin/env python3
"""F6-01: file transfer limits are the transfer modes, not 50 MiB (Automatic <= 2 GB, Unlimited unbounded).

  w10_transfer_modes.py CASE FROM TO [--bytes N]      (FROM, TO in L,M,W; sender must be L or M)

  big       an Automatic send of --bytes (default 1,500,000,000) arrives with the same sha256 (F6-01:big:L>M)
  over      an Automatic send of 2,100,000,000 bytes (sparse, so no disk or hash time) is refused before any byte moves,
            with the 2 GB message pointing at --unlimited, and nothing reaches the receiver's Inbox (F6-01:over:L>M)
  unlimited `pb send --unlimited` of --bytes (default 2,100,000,000) arrives with the same sha256 (F6-01:unlimited:L>M)

Needs the harness environment of tools/device-smoke/xplat_fleet.py (fleet.env, SMOKE_WINDOWS_PASSWORD for W, XPLAT_ALLOW_FLEET_MAC=1 for
the fleet Mac, which the caller holds fleet-mac.lock for). Files are named w10-<case>-<stamp>.bin and removed from both machines.
Prints one JSON line.
"""
import json
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import xplat_fleet as fleet  # noqa: E402


def make(dev, path, size, sparse):
    if dev.os == 'windows':
        raise SystemExit('the sender must be L or M')
    command = 'truncate -s %d %s' % (size, path) if sparse else 'head -c %d /dev/urandom > %s' % (size, path)
    done = dev.run(['sh', '-c', command], timeout=900)
    assert done.rc == 0, 'make failed: ' + done.errtext[-200:]


def sha(dev, path):
    """The hash computed on the device (Unix.sha256 would read the whole file over ssh)."""
    if dev.os == 'windows':
        done = dev.sh("(Get-FileHash -Algorithm SHA256 -LiteralPath '%s').Hash.ToLower()" % path, timeout=1800)
    else:
        done = dev.run(['sh', '-c', 'sha256sum "$1" 2>/dev/null || shasum -a 256 "$1"', '_', path], timeout=1800)
    words = done.text.split()
    return words[0] if done.rc == 0 and words else None


def inbox(dev):
    return json.loads(dev.pb('inbox', 'path', '--json').text)['data']['path']


def main():
    case, src_key, dst_key = sys.argv[1:4]
    size = int(sys.argv[sys.argv.index('--bytes') + 1]) if '--bytes' in sys.argv else {'big': 1_500_000_000, 'over': 2_100_000_000, 'unlimited': 2_100_000_000}[case]
    devices = fleet.fleet(tuple(sorted({src_key, dst_key})))
    src, dst = devices[src_key], devices[dst_key]
    name = 'w10-%s-%d.bin' % (case, int(time.time()))
    path = src.home + '/' + name
    result = {'check': 'F6-01:%s:%s>%s' % (case, src_key, dst_key), 'bytes': size, 'name': name}
    received = None
    try:
        make(src, path, size, sparse=case == 'over')
        args = ['send', path, '--to', dst.name, '--json']
        if case == 'unlimited':
            args.insert(2, '--unlimited')
        started = time.time()
        done = src.pb(*args, timeout=3300)
        seconds = time.time() - started
        text = (done.text + done.errtext)
        received = inbox(dst) + ('\\' if dst.os == 'windows' else '/') + name
        arrived = dst.exists(received)
        result.update(rc=done.rc, seconds=round(seconds), arrived=arrived, tail=text.strip()[-400:])
        if case == 'over':
            ok = done.rc != 0 and not arrived and '2 GB' in text and '--unlimited' in text
        else:
            want = sha(src, path)
            got = sha(dst, received) if arrived else None
            result.update(sha256_source=want, sha256_received=got, mb_per_s=round(size / 1e6 / max(seconds, 1), 1))
            ok = done.rc == 0 and want is not None and want == got
        result['status'] = 'PASS' if ok else 'FAIL'
    finally:
        for dev, target in ((src, path), (dst, received)):
            if target:
                try:
                    dev.remove(target)
                except Exception as error:  # a leftover is reported, never hidden
                    result['cleanup_error_' + dev.name] = str(error)[:120]
    print(json.dumps(result))
    return 0 if result.get('status') == 'PASS' else 1


if __name__ == '__main__':
    sys.exit(main())
