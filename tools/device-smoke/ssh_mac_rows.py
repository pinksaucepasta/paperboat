#!/usr/bin/env python3
"""F5-07..F5-13 managed-SSH checks with a Mac target and a Linux client (macOS twins of tools/device-smoke/ssh_*.sh).

TARGET is the paired Mac (alias --alias, default apple, setup user --mac-user, default admin); CLIENT is a paired Linux
machine (a container) where `pb ssh <alias>` runs. Both are reached through wrappers that take ONE shell-string argument:
  MAC_CMD     runs it on the Mac (keychain unlocked); sudo there reads its password from $SUDO_PW_FILE (on the Mac)
  CLIENT_CMD  runs it on the client (needs passwordless sudo only for the outage step)
Usage: MAC_CMD=... CLIENT_CMD=... ssh_mac_rows.py [--only setup,outage,refusals,revoke,otheruser,audit,forwarding]
Steps with no safe Mac equivalent print SKIP <reason>. The Mac's sshd is only ever stopped by a detached root script
that restarts it on a timer; the pf block and the daemon SIGSTOP likewise have a timed failsafe.
Prints one JSON line; exit 1 on any FAIL.
"""
import argparse, json, os, re, secrets, shlex, subprocess, sys, time

ANCHOR = 'com.apple/pbsmoke'
SSHD_PLIST = '/System/Library/LaunchDaemons/ssh.plist'
ARGS = None
RESULTS = []


def run(env, script, timeout=300):
    cmd = shlex.split(os.environ[env]) + ['bash -c ' + shlex.quote(script)]
    try:
        p = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout, stdin=subprocess.DEVNULL)
    except subprocess.TimeoutExpired:
        return 124, 'timeout after %ds' % timeout
    return p.returncode, p.stdout + p.stderr


def mac(script, timeout=120):
    return run('MAC_CMD', script, timeout)[1]


def daemon_pids():
    """The Mac daemon's pid to pause: launchd's job for its label, never a name match, which also
    finds the user's own `pb daemon ...` commands."""
    match = re.search(r'^\s*pid = (\d+)$', mac('launchctl print gui/$(id -u)/com.pinksaucepasta.paperboatd'), re.MULTILINE)
    return [match.group(1)] if match else []


def cl(script, timeout=300):
    return run('CLIENT_CMD', script, timeout)[1]


def msudo(script, bg=False, timeout=120):
    # SUDO_PW_FILE is a path on the Mac, set here on the controller: expand it before the command leaves
    c = "sudo -S -p '' sh -c %s < %s" % (shlex.quote(script), shlex.quote(os.environ['SUDO_PW_FILE']))
    return mac(('nohup ' + c + ' >/dev/null 2>&1 &') if bg else c, timeout)


def prelude(wait_ssh=True):
    s = ('PB=~/.local/bin/pb; A=%s; D=; for i in $(seq 1 60); do D=$(grep -o "Host \\*\\.[^ ]*" ~/.ssh/paperboat_config 2>/dev/null'
         ' | head -1 | sed "s/Host \\*\\.//"); [ -n "$D" ] && break; sleep 2; done; HN=$A.$D; H=%s@$HN; echo domain=$D; '
         # the client's unix user (pbt) is not an account on the Mac: plain ssh logs in as the Mac setup user
         % (shlex.quote(ARGS.alias), shlex.quote(ARGS.mac_user)))
    if wait_ssh:
        s += ('for i in $(seq 1 60); do $PB machine list 2>/dev/null | grep -q "$A " && break; sleep 2; done; '
              'for i in $(seq 1 30); do ssh -o BatchMode=yes $H true </dev/null >/dev/null 2>&1 && break; sleep 2; done; ')
    return s


def report(row, status, detail=''):
    RESULTS.append({'row': row, 'status': status, 'detail': detail})
    print('%s %s %s' % (status, row, detail), flush=True)


def check_warm(row):
    out = cl(prelude() + 'ssh $H echo warm-ok </dev/null 2>&1')
    if 'warm-ok' not in out:
        report(row, 'FAIL', 'no session before the check: ' + out[-200:].replace('\n', ' '))
        return False
    return True


def setup():
    if not check_warm('F5-07 setup'):
        return
    out = cl(prelude() + r'''ms() { s=$(date +%s%N); timeout 30 "$@" </dev/null >/dev/null 2>&1 || { echo -1; return; }; e=$(date +%s%N); echo $(( (e-s)/1000000 )); }
plain=; pb=; for i in 1 2 3 4 5 6 7; do a=$(ms ssh $H true); b=$(ms $PB ssh $A -- true); echo "RUN $i plain=$a pb=$b"; plain="$plain $a"; pb="$pb $b"; done
p50() { tr ' ' '\n' | grep -v '^$' | sort -n | sed -n 4p; }
echo "RESULT plain_p50=$(echo $plain | p50) pb_p50=$(echo $pb | p50) plain=[$plain ] pb=[$pb ]"''', 600)
    m = re.search(r'RESULT plain_p50=(-?\d+) pb_p50=(-?\d+) plain=\[([^\]]*)\] pb=\[([^\]]*)\]', out)
    ok = m and 0 <= int(m[1]) < 1000 and 0 <= int(m[2]) < 1000 and '-1' not in (m[3] + ' ' + m[4]).split()
    report('F5-07 setup', 'PASS' if ok else 'FAIL', m[0] if m else out[-300:].replace('\n', ' '))


def outage():
    row = 'F5-08 outage'
    if 'sudo-ok' not in cl('sudo -n true && command -v ip >/dev/null && echo sudo-ok'):
        return report(row, 'SKIP', 'client has no passwordless sudo/ip to blackhole the control plane')
    if not check_warm(row):
        return
    api = cl(r'''sed -n 's/.*"server_url": *"https:\/\/\([^"\/]*\).*/\1/p' ~/.config/paperboat/config.json''').strip()
    addrs = sorted(set(cl("getent ahosts %s | awk '{print $1}'" % shlex.quote(api)).split())) if api else []
    if not addrs:
        return report(row, 'SKIP', 'could not resolve the control plane address on the client')
    rule = 'block drop out quick proto { tcp udp } to { %s }' % ' '.join(addrs)
    token = None
    res = ''
    try:
        msudo('sleep 150; /sbin/pfctl -a %s -F all' % ANCHOR, bg=True)  # root failsafe on the Mac
        for a in addrs:
            cl('sudo -n ip route add blackhole %s 2>/dev/null || sudo -n ip -6 route add blackhole %s' % (a, a))
        out = msudo('printf "%%s\\n" %s | /sbin/pfctl -a %s -f - 2>&1; /sbin/pfctl -E 2>&1; /sbin/pfctl -F states 2>&1' % (shlex.quote(rule), ANCHOR))
        t = re.search(r'Token : (\d+)', out)
        token = t[1] if t else None
        time.sleep(45)
        res = cl(prelude(False) + r'''plain=$(ssh $H echo outage-plain-ok </dev/null 2>&1 | tr '\n' ' '); pbo=$($PB ssh $A -- echo outage-pb-ok </dev/null 2>&1 | tr '\n' ' ')
api=$(curl -s -m 5 -o /dev/null -w '%%{http_code}' https://%s/ || true); echo "RESULT api=$api plain=[$plain] pb=[$pbo]"''' % api, 400)
    finally:
        msudo('/sbin/pfctl -a %s -F all%s' % (ANCHOR, '; /sbin/pfctl -X %s' % token if token else ''))
        for a in addrs:
            cl('sudo -n ip route del blackhole %s 2>/dev/null || sudo -n ip -6 route del blackhole %s' % (a, a))
    d = ' '.join(l for l in res.splitlines() if l.startswith('RESULT'))
    ok = 'api=000' in d and re.search(r'plain=\[[^\]]*outage-plain-ok', d) and re.search(r'pb=\[[^\]]*outage-pb-ok', d)
    report(row, 'PASS' if ok else 'FAIL', d or res[-300:].replace('\n', ' '))


def wait_sshd():
    for _ in range(30):
        if 'up' in mac('echo up'):
            return True
        time.sleep(5)
    return False


def case(name):
    return cl(prelude(False) + 'echo "CASE %s [$(timeout 90 ssh $H true </dev/null 2>&1 | tr "\\n" " " | cut -c1-300)]"' % name, 150)


def refusals():
    row = 'F5-09 refusals'
    if not check_warm(row):
        return
    res = ''
    try:  # one detached root script stops sshd, waits, and starts it again, whatever happens to us
        msudo('launchctl bootout system/com.openssh.sshd; sleep 40; launchctl bootstrap system %s' % SSHD_PLIST, bg=True)
        time.sleep(4)
        res += case('sshd_stopped')
    finally:
        back = wait_sshd()
    if not back:
        return report(row, 'FAIL', 'sshd did not come back; the root timer should have restarted it')
    time.sleep(5)
    pids = daemon_pids()
    if pids:  # SIGSTOP the Mac's daemon (docker pause equivalent), SIGCONT on a timer
        mac('nohup sh -c "sleep 50; kill -CONT %s" >/dev/null 2>&1 &' % ' '.join(pids))
        try:
            mac('kill -STOP %s' % ' '.join(pids))
            res += case('machine_offline')
        finally:
            mac('kill -CONT %s' % ' '.join(pids))
    else:
        report(row + ' machine_offline', 'SKIP', 'no "pb daemon" process found on the Mac to stop')
    if 'rc=0' in cl('XDG_RUNTIME_DIR=/run/user/$(id -u) systemctl --user stop paperboatd 2>&1; echo rc=$?'):
        try:
            res += case('service_stopped')
        finally:
            cl('XDG_RUNTIME_DIR=/run/user/$(id -u) systemctl --user start paperboatd')
    else:
        report(row + ' service_stopped', 'SKIP', 'client has no systemd user paperboatd to stop')
    want = {'sshd_stopped': r"pb: (the machine's SSH server closed|SSH is not set up)", 'machine_offline': r'pb: Paperboat could not reach ',
            'service_stopped': r"pb: this computer.s Paperboat service is not running"}
    ran = re.findall(r'CASE (\w+) ', res)
    bad = [k for k in ran if not re.search(r'CASE %s \[[^\]]*%s' % (k, want[k]), res)]
    report(row, 'PASS' if ran and not bad else 'FAIL', ' '.join(l for l in res.splitlines() if l.startswith('CASE'))[:600])


REVOKE = r'''A=%s; PB=~/.local/bin/pb
"$PB" ssh "$A" -- 'echo session-up; sleep 300' > ~/slr.out 2> ~/slr.err & pid=$!
for i in $(seq 1 60); do grep -q session-up ~/slr.out && break; sleep 1; done
grep -q session-up ~/slr.out || { echo "RESULT no-session $(tr '\n' ' ' < ~/slr.err | cut -c1-200)"; exit 0; }
start=$(date +%%s.%%N); "$PB" logout >/dev/null 2>&1
for i in $(seq 1 600); do kill -0 $pid 2>/dev/null || break; sleep 0.1; done
end=$(date +%%s.%%N); alive=$(kill -0 $pid 2>/dev/null && echo yes || echo no); kill $pid 2>/dev/null
echo "RESULT seconds=$(python3 -c "print(round($end-$start,1))") alive=$alive stderr=[$(tr '\n' ' ' < ~/slr.err | cut -c1-200)]"'''


def revoke():
    # `pb logout` signs the client out for good, so it only runs on a FRESH throwaway client (REVOKE_CLIENT_CMD).
    if not os.environ.get('REVOKE_CLIENT_CMD'):
        report('F5-10 revoke', 'SKIP', 'set REVOKE_CLIENT_CMD to a fresh throwaway client; logout would sign out CLIENT_CMD')
        return
    _, out = run('REVOKE_CLIENT_CMD', REVOKE % shlex.quote(ARGS.alias), timeout=240)
    m = re.search(r'RESULT seconds=([0-9.]+) alive=(\w+)', out)
    ok = m and float(m.group(1)) <= 15 and m.group(2) == 'no' and re.search(r'access (to \S+ )?was revoked', out, re.I)
    report('F5-10 revoke', 'PASS' if ok else 'FAIL', out.strip()[-300:])


OTHER_USER = r'''B=%s; U=%s
fail=; ok=; skip=
check() { n=$1; w=$2; shift 2; o=$("$@" 2>&1 </dev/null); if grep -q -- "$w" <<<"$o"; then ok="$ok $n"; else fail="$fail $n($(tr '\n' ' ' <<<"$o" | cut -c1-160))"; fi; }
check setup-user-pb ok-managed $PB ssh $A -- echo ok-managed
check setup-user-plain ok-managed ssh $U@$HN echo ok-managed
check bob-key-plain ok-bob-key ssh -i ~/bob_key -o IdentitiesOnly=yes -o BatchMode=yes $B@$HN echo ok-bob-key
eval "$(ssh-agent -s)" >/dev/null; ssh-add -q ~/bob_key
check bob-agent-pb ok-bob-agent $PB ssh $B@$A -- echo ok-bob-agent
check bob-agent-plain ok-bob-agent ssh -o BatchMode=yes $B@$HN echo ok-bob-agent
ssh-agent -k >/dev/null 2>&1; unset SSH_AUTH_SOCK
if command -v sshpass >/dev/null; then export SSHPASS; SSHPASS=$(cat ~/bob_pw)
check bob-password-plain ok-bob-password sshpass -e ssh -o PubkeyAuthentication=no -o PreferredAuthentications=password,keyboard-interactive $B@$HN echo ok-bob-password
check bob-password-pb ok-bob-password sshpass -e $PB ssh $B@$A -- echo ok-bob-password
# macOS sshd re-prompts after a wrong password, so sshpass stops with exit 5 (incorrect password) and
# prints nothing; either sshd's text or that exit means sshd itself refused the password
SSHPASS=wrong-password-1; o=$(sshpass -e ssh -o PubkeyAuthentication=no -o NumberOfPasswordPrompts=1 $B@$HN echo no 2>&1 </dev/null); rc=$?
if grep -q "Permission denied" <<<"$o" || [ $rc = 5 ]; then ok="$ok bob-wrong-password-is-sshd"; else fail="$fail bob-wrong-password-is-sshd(rc=$rc $(tr '\n' ' ' <<<"$o" | cut -c1-120))"; fi
else skip=" password-checks(no sshpass on the client)"; fi
echo "RESULT ok:[$ok ] fail:[$fail ] skip:[$skip ]"'''


def otheruser():
    row = 'F5-11 otheruser'
    if not check_warm(row):
        return
    bob = 'pbbob' + secrets.token_hex(3)
    pw = secrets.token_hex(10)  # throwaway account, deleted below; visible in process lists while it exists
    out = ''
    try:
        pub = cl('umask 077; rm -f ~/bob_key ~/bob_key.pub; ssh-keygen -q -t ed25519 -N "" -f ~/bob_key && cat ~/bob_key.pub && rm -f ~/bob_key.pub').strip()
        if not pub.startswith('ssh-ed25519'):
            return report(row, 'FAIL', "could not make bob's key on the client")
        cl('umask 077; printf %%s %s > ~/bob_pw' % pw)
        msudo('sysadminctl -addUser {b} -fullName {b} -password {p} -home /Users/{b} && (createhomedir -c -u {b} >/dev/null 2>&1; mkdir -p /Users/{b}; chown {b}:staff /Users/{b}; chmod 750 /Users/{b}); '
              '(dscl . -read /Groups/com.apple.access_ssh >/dev/null 2>&1 && dseditgroup -o edit -a {b} -t user com.apple.access_ssh); '
              'install -d -m 700 -o {b} /Users/{b}/.ssh && printf "%s\\n" {k} > /Users/{b}/.ssh/authorized_keys '
              '&& chown {b} /Users/{b}/.ssh/authorized_keys && chmod 600 /Users/{b}/.ssh/authorized_keys'.format(b=bob, p=pw, k=shlex.quote(pub)))
        out = cl(prelude() + OTHER_USER % (bob, ARGS.mac_user), 600)
    finally:
        cl('rm -f ~/bob_key ~/bob_pw')
        msudo('sysadminctl -deleteUser {b} >/dev/null 2>&1; dscl . -delete /Users/{b} 2>/dev/null; rm -rf /Users/{b}'.format(b=bob))
    m = re.search(r'RESULT ok:\[([^\]]*)\] fail:\[([^\]]*)\] skip:\[([^\]]*)\]', out)
    need = {'setup-user-pb', 'setup-user-plain', 'bob-key-plain', 'bob-agent-pb', 'bob-agent-plain'}
    if m and not m[3].strip():
        need |= {'bob-password-plain', 'bob-password-pb', 'bob-wrong-password-is-sshd'}
    ok = m and not m[2].strip() and need <= set(m[1].split())
    if m and m[3].strip():
        report(row + ' password', 'SKIP', m[3].strip())
    report(row, 'PASS' if ok else 'FAIL', m[0] if m else out[-300:].replace('\n', ' '))


def find_machine_id(node):
    if isinstance(node, dict):
        if ARGS.alias in (node.get('alias'), node.get('name')):
            for k in ('id', 'machine_id'):
                if str(node.get(k, '')).startswith('machine_'):
                    return node[k]
        node = list(node.values())
    if isinstance(node, list):
        for v in node:
            r = find_machine_id(v)
            if r:
                return r


def audit():
    row = 'F5-12 audit'
    if not check_warm(row):
        return
    try:
        mid = find_machine_id(json.loads(cl('~/.local/bin/pb machine list --json 2>/dev/null')))
    except ValueError:
        mid = None
    if not mid:
        return report(row, 'SKIP', 'could not read the Mac machine id from `pb machine list --json` on the client')
    began = time.time()
    marker = 'ssh-audit-%d' % began
    if marker not in cl('~/.local/bin/pb ssh %s -- echo %s </dev/null 2>&1' % (shlex.quote(ARGS.alias), marker)):
        return report(row, 'FAIL', 'pb ssh did not run')
    db = os.environ.get('PB_AUDIT_DB_HOST', 'root@coolify')
    query = ("SELECT event_type, metadata->>'origin', metadata->>'session_id', metadata->>'outcome', coalesce(metadata->>'bytes_in','0'), "
             "coalesce(metadata->>'bytes_out','0'), metadata->>'peer_endpoint_id', idempotency_key FROM paperboat.audit_events WHERE resource_id='%s' "
             "AND event_type IN ('machine.access_started','machine.access_ended') AND metadata->>'capability'='managed_ssh' "
             "AND created_at > now() - interval '%d seconds' ORDER BY created_at, id;" % (mid, int(time.time() - began) + 10))
    rows = ''
    for _ in range(30):  # the host uploads within its 30 s cadence
        rows = subprocess.run(['ssh', db, 'docker exec -i paperboat-postgres-1 psql -U postgres -d paperboat -qAt -F "|"'], input=query,
                              capture_output=True, text=True).stdout
        if re.search(r'^machine.access_ended\|', rows, re.M):
            break
        time.sleep(5)
    recs = [r.split('|') for r in rows.splitlines() if r]
    starts = [r for r in recs if r[0] == 'machine.access_started']
    ends = [r for r in recs if r[0] == 'machine.access_ended']
    ids = {s[7].removeprefix('offline:') for s in starts}
    ok = bool(starts and ends) and all(e[1] == 'online' and e[3] == 'closed' and int(e[4]) > 0 and int(e[5]) > 0 and e[2] in ids for e in ends) \
        and all(s[1] == 'online' and s[6].startswith('session_') for s in starts)
    detail = ' '.join('%s:%s:%s:%s:%s' % (r[0][8:], r[2][:8], r[3], r[4], r[5]) for r in recs)
    report(row, 'PASS' if ok else 'FAIL', detail[:600])


FORWARDING = r'''fail=; ok=
check() { n=$1; w=$2; shift 2; o=$("$@" 2>&1 </dev/null); if grep -q -- "$w" <<<"$o"; then ok="$ok $n"; else fail="$fail $n($(tr '\n' ' ' <<<"$o" | cut -c1-160))"; fi; }
banner() { for i in $(seq 1 30); do timeout 5 bash -c "exec 3<>/dev/tcp/127.0.0.1/$1; head -c 7 <&3" 2>/dev/null && return; sleep 1; done; echo "no listener on $1"; }
ssh -o ExitOnForwardFailure=yes -L 127.0.0.1:12222:127.0.0.1:22 $H sleep 40 </dev/null >/dev/null 2>&1 &
check port-forward-plain SSH-2.0 banner 12222
kill %1 2>/dev/null; wait
check agent-forward-plain ED25519 ssh -A $H ssh-add -l
check no-x11-plain "display=<>" env DISPLAY=:0 ssh $H 'echo "display=<$DISPLAY>"'
check no-x11-pb "display=<>" env DISPLAY=:0 $PB ssh $A -- 'echo "display=<$DISPLAY>"'
echo "RESULT ok:[$ok ] fail:[$fail ]"'''


def forwarding():
    row = 'F5-13 forwarding'
    if not check_warm(row):
        return
    out = cl(prelude() + FORWARDING, 600)
    want = 'RESULT ok:[ port-forward-plain agent-forward-plain no-x11-plain no-x11-pb ] fail:[ ]'
    report(row, 'PASS' if want in out else 'FAIL', ' '.join(l for l in out.splitlines() if l.startswith('RESULT')) or out[-300:].replace('\n', ' '))


STEPS = {'setup': setup, 'outage': outage, 'refusals': refusals, 'revoke': revoke, 'otheruser': otheruser, 'audit': audit, 'forwarding': forwarding}


def main():
    global ARGS
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument('--only', default=','.join(STEPS))
    p.add_argument('--alias', default='apple')
    p.add_argument('--mac-user', default='admin')
    ARGS = p.parse_args()
    names = ARGS.only.split(',')
    unknown = [n for n in names if n not in STEPS]
    if unknown or not all(os.environ.get(v) for v in ('MAC_CMD', 'CLIENT_CMD')):
        sys.exit('unknown step %s or MAC_CMD/CLIENT_CMD unset' % unknown)
    for n in names:
        try:
            STEPS[n]()
        except Exception as e:  # a crashed step is a FAIL, the rest still run
            report(n, 'FAIL', 'crashed: %r' % e)
    failed = any(r['status'] == 'FAIL' for r in RESULTS)
    print(json.dumps({'check': 'ssh-mac-rows', 'status': 'FAIL' if failed else 'PASS', 'results': RESULTS}))
    sys.exit(failed)


if __name__ == '__main__':
    main()
