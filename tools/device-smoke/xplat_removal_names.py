#!/usr/bin/env python3
"""X-41 (F1-09, F2-05, local names part): the browser name of a machine (<alias>.<local domain>) on the other two OSes.

  xplat_removal_names.py [--targets L,M,W]     (env of xplat_removal.py)

Each observer answers HTTP 200 on http://<alias>.<domain>/ for a peer of its account and 421 for a name that is none. After an
observer revokes the target neither observer answers 200 for it within 2 minutes (the name goes back to 421); after the target is
enrolled again both answer 200 again within 3 minutes. Results: results/xplat-removal-names-<stamp>.json, ids `X-41-NAMES:<target>`.
"""
import json, sys, time
from pathlib import Path
sys.path.insert(0, str(Path(__file__).parent))
import xplat_fleet as fleet
import xplat_removal as rem

results = {}


def domain(dev):
    for line in dev.pb('config', 'local-access', 'show').text.splitlines():
        if line.startswith('domain:'):
            return line.split(':', 1)[1].strip()
    raise AssertionError('%s: no local domain' % dev.name)


def code(dev, name):
    curl = 'curl.exe' if dev.os == 'windows' else 'curl'
    done = dev.run([curl, '-s', '-m', '10', '-o', '/dev/null' if dev.os != 'windows' else 'NUL', '-w', '%{http_code}', 'http://%s/' % name], timeout=60)
    return done.text.strip()[-3:]


def wait_code(dev, name, want, seconds):
    began, last = time.time(), ''
    while time.time() - began < seconds:
        last = code(dev, name)
        if (last == '200') == want:
            return int(time.time() - began)
        time.sleep(5)
    raise AssertionError('%s: %s answered %s after %d s (wanted %s)' % (dev.name, name, last, seconds, '200' if want else 'not 200'))


def flow(target, observers, controller):
    name = '%s.%s' % (target.alias.split('.')[0], domain(observers[0]))
    old_id, _ = rem.identity(observers[0], target.alias)
    for o in observers:
        wait_code(o, name, True, 60)
    rem.confirm_revoke(observers[0], old_id)
    gone = [wait_code(o, name, False, 120) for o in observers]
    rem.enroll_again(target, controller, 'reset')
    rem.sign_in(target, TOKEN)
    back = [wait_code(o, name, True, 180) for o in observers]
    return '%s: 200 before, not 200 %s s after the revoke, 200 again %s s after enrolling again' % (name, '/'.join(map(str, gone)), '/'.join(map(str, back)))


def main():
    global TOKEN
    argv = sys.argv[1:]
    targets = argv[argv.index('--targets') + 1].split(',') if '--targets' in argv else ['L', 'M', 'W']
    devices = fleet.fleet(('L', 'M', 'W'))
    TOKEN = rem.mint_api_token()
    for dev in devices.values():
        rem.sign_in(dev, TOKEN)
    for key in targets:
        target = devices[key]
        observers = [d for k, d in devices.items() if k != key]
        controller = next(devices[k] for k in 'LMW' if k != key)
        started = time.time()
        try:
            detail = flow(target, observers, controller)
            results['X-41-NAMES:' + key] = {'status': 'PASS', 'detail': detail}
        except Exception as error:  # noqa: BLE001
            detail = str(error)[:500]
            results['X-41-NAMES:' + key] = {'status': 'FAIL', 'detail': detail}
        print('%s X-41-NAMES:%s %ds %s' % (results['X-41-NAMES:' + key]['status'], key, time.time() - started, detail), flush=True)
        for dev in devices.values():
            try:
                rem.sign_in(dev, TOKEN)
            except Exception as error:  # noqa: BLE001
                print('sign-in of %s failed: %s' % (dev.name, str(error)[:160]), flush=True)
    out = Path(__file__).parent / 'results' / ('xplat-removal-names-%s.json' % time.strftime('%Y%m%dT%H%M%SZ', time.gmtime()))
    out.write_text(json.dumps({'check': 'xplat-removal-names', 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
