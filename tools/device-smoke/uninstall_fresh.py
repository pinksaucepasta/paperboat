#!/usr/bin/env python3
"""uninstall_fresh.py HOST BINARY  (check id: uninstall-fresh, CHECKLIST F1-09 / C021, PB-U1)

A fresh throwaway systemd container on HOST (image test-systemd): BINARY is installed and paired as user pbt (a
throwaway machine of the account HOST's pb is signed in to, or of $FRESH_MINT's), the privileged uninstall inventory
is recorded (it must see the install), then the command a user runs, `pb uninstall --confirmation 'UNINSTALL PAPERBOAT'
--hostname <h> --json`, removes it. Every leftover is listed, none stops the check:
  uninstall_inventory.py (services, files, CA trust, firewall, manuals, credentials, ssh config, processes), a sweep of
  the whole container for anything named paperboat and for a `pb` binary, system and user systemd units, sockets and
  listeners, processes, accounts/groups the installer added, the Inbox (a sentinel file must survive), the server-side
  machine record (gone or revoked). Then a reinstall + re-pair in the same container must end with a healthy doctor.
The container is removed and every machine revoked by exact id on every exit path. Nothing secret is printed or kept
(the enrollment token only travels through 0600 files, fresh_pair_lib.sh pair_user).
Prints one JSON line {"check":"uninstall-fresh","status":PASS|FAIL,"leftovers":[...],"machine_id":...}; exit 1 on FAIL.
env: SMOKE_CT_DOCKER=docker when HOST's user needs no sudo for docker; FRESH_MINT as in fresh_pair_lib.sh.
Not for a paired device: it only ever touches its own container and the machines it paired.
"""
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import time

HERE = Path(__file__).resolve().parent
DOCKER = os.environ.get('SMOKE_CT_DOCKER', 'sudo -n docker')
USER_EXEC = '-u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000'
PB = '/home/pbt/.local/bin/pb'
REGISTRATION = '/home/pbt/.local/state/paperboat/runtime/machine-registration.json'
SENTINEL = '.pb-uninstall-proof'

# Runs as root in the container, prints `LEFT<TAB>kind<TAB>detail` per leftover. The Inbox, and the directories that hold
# it, are the documented exception.
SCAN = r'''
INBOX=${INBOX:?}
left() { printf 'LEFT\t%s\t%s\n' "$1" "$2"; }
kept() { case "$1" in "$INBOX"|"$INBOX"/*) return 0 ;; esac; case "$INBOX/" in "$1"/*) return 0 ;; esac; return 1; }
find / \( -path /proc -o -path /sys -o -path /dev \) -prune -o \
  \( -iname '*paperboat*' -o \( -name pb \( -type f -o -type l \) ! -path /home/pbt/pb \) \) -print 2>/dev/null |
while IFS= read -r p; do
  kept "$p" && continue
  if [ -S "$p" ]; then k=socket
  else case "$p" in
    */pb) k=binary ;;
    /etc/systemd/*|/usr/lib/systemd/*|/run/systemd/*|*/.config/systemd/*|*.service|*.socket|*.timer) k=unit-file ;;
    /etc/ssl/*|*ca-certificates*) k=ca-trust ;;
    *) k=file ;;
  esac; fi
  left "$k" "$p"
done
systemctl list-unit-files --plain --no-legend --no-pager 'paperboat*' 2>/dev/null | while read -r u _; do left unit "system:$u"; done
systemctl list-units --all --plain --no-legend --no-pager 'paperboat*' 2>/dev/null | while read -r u _; do left unit "system-loaded:$u"; done
U="runuser -u pbt -- env XDG_RUNTIME_DIR=/run/user/1000"
$U systemctl --user list-unit-files --plain --no-legend --no-pager 'paperboat*' 2>/dev/null | while read -r u _; do left unit "user:$u"; done
$U systemctl --user list-units --all --plain --no-legend --no-pager 'paperboat*' 2>/dev/null | while read -r u _; do left unit "user-loaded:$u"; done
ss -xanH 2>/dev/null | grep -i paperboat | cut -c1-160 | while IFS= read -r l; do left socket "$l"; done
ss -tulnpH 2>/dev/null | grep -E 'users:\(\("(pb|paperboat[^"]*)"' | cut -c1-160 | while IFS= read -r l; do left listener "$l"; done
ps -eo pid=,comm= | awk '$2=="pb" || tolower($2) ~ /paperboat/ {print $1" "$2}' | while IFS= read -r l; do left process "$l"; done
'''
ACCOUNTS = "getent passwd | cut -d: -f1; echo --; getent group | cut -d: -f1"


def redact(text):
    """No enrollment token in anything printed: the PB_TOKEN value and any 26-character token."""
    text = re.sub(r"PB_TOKEN=['\\]*[A-Za-z0-9]+['\\]*", 'PB_TOKEN=<redacted>', str(text))
    return re.sub(r'\b[0-9A-Z]{26}\b', '<token>', text)


def run_ssh(host, command, stdin=None, timeout=300):
    argv = ['bash', '-c', command] if host == 'local' else ['ssh', '-o', 'BatchMode=yes', host, command]
    try:
        done = subprocess.run(argv, input=stdin, capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        return 124, 'timed out after %ss' % timeout
    return done.returncode, done.stdout + done.stderr


def stage(binary, host, dest):
    return subprocess.run(['bash', '-c', '. "$0/stage_binary.sh"; stage_binary "$1" "$2" "$3"', str(HERE), binary, host, dest]).returncode == 0


def revoke(machine_id):
    done = subprocess.run([sys.executable, str(HERE / 'cleanup_test_account.py'), '--id', machine_id, '--stale', '0 seconds', '--apply'],
                          capture_output=True, text=True)
    return done.returncode == 0


class Check:
    def __init__(self, host, binary):
        self.host, self.binary = host, binary
        self.ct = 'test-pbu1-%d' % int(time.time())
        self.staged = '/tmp/%s.pb' % self.ct
        self.leftovers, self.machine_ids, self.before, self.reinstall, self.digest = [], [], {}, 'not run', ''

    def ssh(self, command, **kwargs):
        return run_ssh(self.host, command, **kwargs)

    def exec(self, script, user=True, stdin=None, env=None, timeout=300):
        flags = USER_EXEC if user else ''
        flags += ''.join(' -e ' + shlex.quote(k + '=' + v) for k, v in (env or {}).items())
        flags += ' -i' if stdin is not None else ''
        return self.ssh('%s exec %s %s sh -c %s' % (DOCKER, flags, self.ct, shlex.quote(script)), stdin=stdin, timeout=timeout)

    def step(self, name, work):
        """Run one stage; its failure becomes a leftover entry, never an early exit. True when it passed."""
        try:
            problem = work()
        except Exception as error:  # noqa: BLE001 - every stage is reported, none aborts the run
            problem = '%s: %s' % (type(error).__name__, error)
        if problem:
            self.leftovers.append(redact('%s: %s' % (name, problem)))
        return not problem

    def start(self):
        d, ct = DOCKER, self.ct
        code, out = self.ssh(
            f"{d} rm -f {ct} >/dev/null 2>&1; {d} run -d --name {ct} --hostname {ct} --privileged --dns-option single-request "
            f"--cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock test-systemd >/dev/null && "
            f"for i in $(seq 30); do {d} exec {ct} systemctl is-system-running 2>/dev/null | grep -qE 'running|degraded' && break; sleep 1; done; "
            f"{d} exec {ct} loginctl enable-linger pbt && {d} exec {ct} systemctl start user@1000.service")
        return None if code == 0 else 'container did not start: ' + out[-200:]

    def pair(self, alias):
        """fresh_pair_lib.sh pair_user: install BINARY as pbt and pair it as ALIAS; one MACHINE_ID= line out."""
        shim = 'sudo() { shift; "$@"; }\n' if DOCKER == 'docker' else ''
        mint = 'FRESH_MINT=%s\n' % shlex.quote(os.environ['FRESH_MINT']) if os.environ.get('FRESH_MINT') else ''
        script = 'set -euo pipefail\nFRESH_BINARY=%s\n%s%s%s\npair_user %s pbt 1000 %s\n' % (
            shlex.quote(self.staged), mint, shim, (HERE / 'fresh_pair_lib.sh').read_text(), self.ct, alias)
        code, out = self.ssh('bash -s', stdin=script, timeout=600)
        found = re.findall(r'^MACHINE_ID=(machine_[0-9a-f-]+)$', out, re.M)
        if found:
            self.machine_ids.append(found[-1])
        return None if code == 0 and found else 'install/pair failed: ' + out[-300:]

    def inventory(self, inbox):
        code, out = self.exec('python3 -I /root/uninstall_inventory.py --user pbt --inbox "$INBOX"', user=False, env={'INBOX': inbox}, timeout=120)
        try:
            return json.loads(out.strip().splitlines()[-1])
        except (ValueError, IndexError):
            return {'status': 'FAIL', 'reason': 'inventory returned no receipt: ' + out[-200:]}

    def accounts(self):
        code, out = self.exec(ACCOUNTS, user=False)
        return set(out.split()) if code == 0 else None

    def machine_listed(self, machine_id):
        mint = os.environ.get('FRESH_MINT') or '$HOME/.local/bin/pb'
        code, out = self.ssh(f'{mint} machine list --all --json', timeout=120)
        try:
            data = json.loads(out)
            data = data.get('data', data)
            return next((m for m in data['machines'] if m['id'] == machine_id), None)
        except (ValueError, KeyError, TypeError):
            raise RuntimeError('machine list gave no usable answer: ' + out[-200:])

    def phase_uninstall(self, alias):
        names = self.accounts()  # before the install: whatever account or group appears by the end of the uninstall is the installer's
        if not self.step('install+pair', lambda: self.pair(alias)):
            return
        machine_id = self.machine_ids[0]
        inbox = ''

        def prepare():
            nonlocal inbox
            code, out = self.exec('python3 -c "import json;print(json.load(open(\'%s\'))[\'inbox_path\'])"' % REGISTRATION)
            inbox = out.strip().splitlines()[-1] if code == 0 and out.strip() else ''
            if not inbox.startswith('/'):
                return 'no Inbox path in the machine registration: ' + out[-200:]
            code, out = self.exec('mkdir -p "$INBOX" && head -c 64 /dev/urandom > "$INBOX/%s" && sha256sum < "$INBOX/%s"' % (SENTINEL, SENTINEL), env={'INBOX': inbox})
            self.digest = out.split()[0] if code == 0 and out.split() else ''
            return None if self.digest else 'could not write the Inbox sentinel: ' + out[-200:]
        if not self.step('inbox', prepare):
            return
        self.step('stage-inventory', lambda: None if self.ssh(f"cat | {DOCKER} exec -i {self.ct} sh -c 'cat > /root/uninstall_inventory.py'",
                                                           stdin=(HERE / 'uninstall_inventory.py').read_text())[0] == 0 else 'could not copy uninstall_inventory.py')

        def positive_control():
            receipt = self.inventory(inbox)
            self.before = receipt.get('leftovers', {})
            return None if receipt.get('status') == 'FAIL' and receipt.get('total', 0) >= 1 else 'inventory saw nothing of the install: ' + json.dumps(receipt)[:200]
        self.step('pre-uninstall-inventory', positive_control)
        uninstall = ("%s uninstall --confirmation 'UNINSTALL PAPERBOAT' --hostname \"$(hostname)\" --json" % PB)
        code, out = self.exec(uninstall, timeout=600)
        self.step('pb uninstall', lambda: self.check_uninstall_result(code, out))
        self.check_leftovers(inbox, machine_id, names)
        self.step('reinstall', lambda: self.phase_reinstall(alias + '-again'))

    @staticmethod
    def check_uninstall_result(code, out):
        try:
            result = json.loads(out.strip().splitlines()[-1])
        except (ValueError, IndexError):
            result = {}
        if code != 0 or result.get('uninstalled') is not True:
            return 'exit %s: %s' % (code, out[-300:])
        if result.get('inbox_preserved') is not True:
            return 'did not report the Inbox preserved'
        if result.get('server_record_removed') is False:
            return 'server record not removed: ' + str(result.get('server_record_notice', ''))[:200]

    def check_leftovers(self, inbox, machine_id, names_before):
        receipt = self.inventory(inbox)
        if receipt.get('status') != 'PASS':
            self.leftovers.extend('inventory:%s=%s' % kv for kv in sorted(receipt.get('leftovers', {}).items()))
            self.leftovers.extend('file:' + path for path in receipt.get('file_paths', []))
            if receipt.get('reason'):
                self.leftovers.append('inventory: ' + str(receipt['reason'])[:200])
        code, out = self.exec(SCAN, user=False, env={'INBOX': inbox}, timeout=180)
        if code != 0 and 'LEFT\t' not in out:
            self.leftovers.append('scan: did not run: ' + out[-200:])
        self.leftovers.extend('%s:%s' % tuple(line.split('\t')[1:3]) for line in out.splitlines() if line.startswith('LEFT\t') and line.count('\t') >= 2)
        after = self.accounts()
        if names_before is not None and after is not None:
            self.leftovers.extend('account:' + name for name in sorted(after - names_before))
        elif names_before is None or after is None:
            self.leftovers.append('accounts: could not be listed')
        code, out = self.exec('sha256sum < "$INBOX/%s"' % SENTINEL, env={'INBOX': inbox})
        if code != 0 or out.split()[:1] != [self.digest]:
            self.leftovers.append('inbox: the sentinel file did not survive unchanged')
        self.step('server-record', lambda: self.record_problem(machine_id))

    def record_problem(self, machine_id):
        entry = self.machine_listed(machine_id)
        if entry and entry.get('state') != 'revoked' and not entry.get('revoked_at'):
            return 'machine %s is still listed (state %s)' % (machine_id, entry.get('state'))

    def phase_reinstall(self, alias):
        problem = self.pair(alias)
        if problem:
            return problem
        deadline = time.monotonic() + 120
        while True:
            code, out = self.exec(PB + ' doctor --json', timeout=90)
            try:
                healthy = json.loads(out).get('overall') == 'healthy'
            except ValueError:
                healthy = False
            if healthy:
                self.reinstall = 'doctor healthy'
                return None
            if time.monotonic() > deadline:
                self.reinstall = 'doctor unhealthy'
                return 'doctor was not healthy within 120 s after the reinstall'
            time.sleep(5)

    def run(self):
        try:
            if not self.step('staging', lambda: None if stage(self.binary, self.host, self.staged) else 'could not copy the build to ' + self.host):
                return
            if self.step('container', self.start):
                self.phase_uninstall(self.ct)
        finally:
            self.ssh(f'{DOCKER} rm -f {self.ct} >/dev/null 2>&1; rm -f {self.staged}')
            for machine_id in self.machine_ids:
                self.step('teardown', lambda: None if revoke(machine_id) else 'revoke of %s failed' % machine_id)

    def line(self):
        status = 'FAIL' if self.leftovers else 'PASS'
        return json.dumps({'check': 'uninstall-fresh', 'status': status, 'leftovers': self.leftovers,
                           'machine_id': self.machine_ids[0] if self.machine_ids else None, 'machine_ids': self.machine_ids,
                           'before': self.before, 'reinstall': self.reinstall, 'container': self.ct}), status


def main(argv):
    if len(argv) != 3:
        print(__doc__.splitlines()[0], file=sys.stderr)
        return 2
    check = Check(argv[1], argv[2])
    check.run()
    line, status = check.line()
    print(redact(line), flush=True)
    return 0 if status == 'PASS' else 1


if __name__ == '__main__':
    sys.exit(main(sys.argv))
