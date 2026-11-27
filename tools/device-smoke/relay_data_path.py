#!/usr/bin/env python3
"""relay-data-path: bytes carried by a relay between two real machines arrive complete and unchanged.

  relay_data_path.py [--devices L,M,W] [--mib 32]        (environment of xplat_fleet.py)

`pb` cannot pin a daemon to one relay node, so this uses the pairs that are on a relay by themselves (machines behind
different NATs, such as two hosted runners): for every pair with a Unix client whose `pb ping` reports a relay path,
--mib MiB go up (exec stdin, hashed by the target) and --mib MiB come down (target stdout, hashed here), both compared
by SHA-256, and `pb ping` must still report a relay path afterwards. FAIL when no pair at all is on a relay.
"""
import hashlib
import json
import sys
import time
from itertools import permutations
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import xplat_fleet as fleet  # noqa: E402
from results_dir import results_dir  # noqa: E402

BLOCK = hashlib.sha256(b'paperboat relay data path').digest() * 2048  # 64 KiB, not compressible to nothing


def paths(src, dst):
    done = src.pb('ping', dst.alias, '--json', timeout=120)
    value = json.loads(done.text)
    return sorted({s['path'] for s in value.get('samples', []) if not s.get('lost')})


def python(device):
    return 'python' if device.os == 'windows' else 'python3'


def transfer(src, dst, size):
    payload = (BLOCK * (size // len(BLOCK) + 1))[:size]
    want = hashlib.sha256(payload).hexdigest()
    started = time.time()
    up = src.run([src.pb_path, 'exec', dst.alias, '--', python(dst), '-c',
                  'import sys,hashlib;b=sys.stdin.buffer.read();print(len(b),hashlib.sha256(b).hexdigest())'], input=payload, timeout=600)
    assert up.rc == 0 and up.text.split() == [str(size), want], 'upload: exit %d, got %r %s' % (up.rc, up.text[-200:], up.errtext[-300:])
    up_seconds = time.time() - started
    code = ('import sys,hashlib;k=hashlib.sha256(b"paperboat relay data path").digest()*2048;'
            'sys.stdout.buffer.write((k*(%d//len(k)+1))[:%d])' % (size, size))
    started = time.time()
    down = src.run([src.pb_path, 'exec', dst.alias, '--', python(dst), '-c', code], timeout=600)
    assert down.rc == 0 and len(down.out) == size and hashlib.sha256(down.out).hexdigest() == want, \
        'download: exit %d, %d of %d bytes %s' % (down.rc, len(down.out), size, down.errtext[-300:])
    return up_seconds, time.time() - started


def main():
    argv = sys.argv[1:]
    size = int(argv[argv.index('--mib') + 1] if '--mib' in argv else 32) << 20
    devices = fleet.fleet()
    results, relayed = [], 0
    for a, b in permutations(devices, 2):
        src, dst, key = devices[a], devices[b], 'relay-data-path:%s>%s' % (a, b)
        if src.os == 'windows':
            continue
        try:
            before = paths(src, dst)
            if not before or not all('relay' in p for p in before):
                results.append({'id': key, 'status': 'n/a', 'detail': 'this pair is not on a relay: %s' % before})
            else:
                relayed += 1
                up, down = transfer(src, dst, size)
                after = paths(src, dst)
                assert after and all('relay' in p for p in after), 'the path changed during the transfer: %s -> %s' % (before, after)
                results.append({'id': key, 'status': 'PASS', 'detail': '%d MiB up in %.0f s and down in %.0f s, SHA-256 equal, path %s'
                                % (size >> 20, up, down, after)})
        except Exception as error:  # noqa: BLE001 - every pair is reported
            results.append({'id': key, 'status': 'FAIL', 'detail': str(error)[:600]})
        print('%(status)s %(id)s %(detail)s' % results[-1], flush=True)
    if not relayed and not any(r['status'] == 'FAIL' for r in results):
        results.append({'id': 'relay-data-path', 'status': 'FAIL', 'detail': 'no pair of these machines is on a relay path'})
        print('FAIL relay-data-path no pair of these machines is on a relay path')
    out = results_dir(__file__) / ('relay-data-path-%s.json' % time.strftime('%Y%m%dT%H%M%SZ', time.gmtime()))
    out.write_text(json.dumps({'check': 'relay-data-path', 'results': results}, indent=1))
    failed = [r['id'] for r in results if r['status'] == 'FAIL']
    print('%s %s' % ('FAIL' if failed else 'PASS', out))
    sys.exit(1 if failed else 0)


if __name__ == '__main__':
    main()
