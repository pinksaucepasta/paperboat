#!/usr/bin/env python3
"""F14-03 on a Unix device (a macpool Mac) reached over ssh: a failed restart and a full disk during `pb update --from`.

  update_faults.py --device macpool-8 --peer anvit@hp --machine NAME --build PATH --other PATH [restart-failure] [disk-full]

--build is the newer pb on the device (the restart-failure check ends installing it) and --other is the build that was installed
first (the disk-full check ends installing that one), so each update changes the version.
Failed restart: a watcher on the device kills the daemon roles of the build being installed as soon as it is in the
active slot (identified by its sha256), after the previous services were stopped. The update must end with
update_rolled_back and a plain message, the previous version must run again (version, doctor, exec from the peer, one pin,
the same launchd jobs), and a clean retry installs the build.
Full disk: the system disk is filled to 30 MB (the update cannot be copied for the install) and to 140 MB (the protected
copy of the runtime fails); each ends with insufficient_disk_space and a message naming the disk, nothing changed (version,
doctor, exec, no staging directory left), the fill file is removed, and an update with space succeeds.
Prints one JSON line per step and exits non-zero on the first failure. Never prints tokens, passwords or file contents.
"""
import argparse
import json
import secrets
import subprocess
import sys
import time

SSH = ['ssh', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=20', '-o', 'ServerAliveInterval=20']
PB = '~/.local/bin/pb'
SLOT = '/Library/PrivilegedHelperTools/Paperboat/users/*/bin/pb'
WATCH = r'''SHA=$1; END=$((SECONDS+${2:-900}))
while [ $SECONDS -lt $END ]; do
  for BIN in /Library/PrivilegedHelperTools/Paperboat/users/*/bin/pb; do
    if [ "$(shasum -a 256 "$BIN" 2>/dev/null | cut -d' ' -f1)" = "$SHA" ]; then sudo pkill -KILL -f "$BIN daemon" 2>/dev/null; fi
  done
  sleep 0.3
done'''


class Failure(Exception):
    pass


def run(argv, timeout=300, stdin=None):
    done = subprocess.run(argv, capture_output=True, text=True, timeout=timeout, stdin=subprocess.DEVNULL if stdin is None else None, input=stdin)
    return done.returncode, done.stdout + done.stderr


class Device:
    def __init__(self, args):
        self.host, self.peer, self.machine = args.device, args.peer, args.machine

    def sh(self, command, timeout=300):
        return run([*SSH, self.host, command], timeout)

    def receipt(self, *arguments, env='', timeout=900):
        code, out = self.sh(f"{env} {PB} {' '.join(arguments)} --json 2>&1", timeout)
        return code, json.loads(out[out.index('{"schema_version"'):])

    def version(self):
        return self.receipt('--version')[1]['data']['version']

    def healthy(self, seconds=240):
        deadline = time.time() + seconds
        while time.time() < deadline:
            doctor = self.sh(f'{PB} doctor 2>&1 | head -n 1', 90)[1]
            marker = 'pbmac-' + secrets.token_hex(4)
            code, out = run([*SSH, self.peer, f'$HOME/.local/bin/pb exec {self.machine} -- echo {marker}'], 120)
            if 'doctor: healthy' in doctor and code == 0 and marker in out:
                return True
            time.sleep(5)
        return False

    def pins(self):
        return self.sh('sudo ls /Library/PrivilegedHelperTools/Paperboat/users/*/releases/pins')[1].split()

    def jobs(self):
        return sorted(line.split()[-1] for line in self.sh('sudo launchctl list | grep com.pinksaucepasta')[1].splitlines() if line.strip())


def step(name, ok, **detail):
    print(json.dumps({'step': name, 'status': 'PASS' if ok else 'FAIL', **detail}), flush=True)
    if not ok:
        raise Failure(name)


def restart_failure(device, build):
    original = device.version()
    good = json.loads(device.sh(f'{build} --version --json')[1])['data']['version']
    assert original != good, 'the installed build and --build are the same version'
    pins, jobs = device.pins(), device.jobs()
    sha = device.sh(f'shasum -a 256 {build} | cut -d" " -f1')[1].strip()
    device.sh(f"cat > ~/reo-watch.sh <<'EOF'\n{WATCH}\nEOF\nchmod +x ~/reo-watch.sh")
    code, receipt = device.receipt('update', '--from', build, env=f'(nohup ~/reo-watch.sh {sha} 1500 >/dev/null 2>&1 & echo $! > ~/reo-watch.pid);')
    device.sh('kill $(cat ~/reo-watch.pid) 2>/dev/null; pkill -f reo-watch.sh; rm -f ~/reo-watch.pid ~/reo-watch.sh')
    error = receipt.get('error', {})
    step('restart-failure-message', receipt.get('ok') is False and error.get('code') == 'update_rolled_back'
         and 'previous version was restored' in error.get('message', '') and error.get('state_changed') is False, error=error.get('code'), message=error.get('message', '')[:120])
    step('restart-failure-previous-version-runs', device.healthy() and device.version() == original, version=original)
    step('restart-failure-nothing-left', device.pins() == pins and device.jobs() == jobs, pins=len(device.pins()), jobs=len(device.jobs()))
    code, receipt = device.receipt('update', '--from', build)
    step('restart-failure-retry-installs', code == 0 and receipt.get('ok') and device.healthy() and device.version() == good, version=good)
    return good


def disk_full(device, build):
    original = device.version()
    other = original
    for free_mb in (30, 140):
        # Fill the system volume to free_mb MB free; the fill file is removed even when the update fails.
        script = ("F=/var/tmp/reo-fill.bin; avail=$(df -k / | awk 'NR==2{print $4}'); "
                  f"count=$(( avail / 1024 - {free_mb} )); sudo dd if=/dev/zero of=$F bs=1m count=$count 2>/dev/null; "
                  f"echo free_mb=$(( $(df -k / | awk 'NR==2{{print $4}}') / 1024 )); {PB} update --from {build} --json 2>&1; sudo rm -f $F")
        code, out = device.sh(script, 1800)
        receipt = json.loads(out[out.index('{"schema_version"'):])
        error = receipt.get('error', {})
        step(f'disk-full-{free_mb}mb-message', error.get('code') == 'insufficient_disk_space' and error.get('state_changed') is False
             and 'not enough free disk space' in error.get('message', ''), error=error.get('code'), message=error.get('message', '')[:120])
        step(f'disk-full-{free_mb}mb-nothing-changed', device.healthy() and device.version() == original, version=original)
        leftover = device.sh("ls -d /tmp/paperboat-update-* ${TMPDIR:-/tmp}/paperboat-update-* /var/tmp/reo-fill.bin 2>/dev/null; sudo ls /Library/PrivilegedHelperTools/Paperboat/users/*/bin/.binary-* 2>/dev/null")[1].split()
        step(f'disk-full-{free_mb}mb-no-leftovers', not leftover, leftover=leftover)
    code, receipt = device.receipt('update', '--from', build)
    step('disk-full-retry-installs', code == 0 and receipt.get('ok') and device.healthy(), version=device.version())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--device', required=True)
    parser.add_argument('--peer', required=True)
    parser.add_argument('--machine', required=True)
    parser.add_argument('--build', required=True, help='path on the device of the newer pb to install')
    parser.add_argument('--other', required=True, help='path on the device of the pb that is installed now')
    parser.add_argument('checks', nargs='*', default=['restart-failure', 'disk-full'])
    args = parser.parse_args()
    device = Device(args)
    try:
        for name in args.checks:
            {'restart-failure': restart_failure, 'disk-full': disk_full}[name](device, args.build if name == 'restart-failure' else args.other)
    except Failure as failure:
        sys.exit(f'FAIL {failure}')
    print(json.dumps({'check': 'update-faults', 'device': args.device, 'status': 'PASS', 'checks': args.checks}), flush=True)


if __name__ == '__main__':
    main()
