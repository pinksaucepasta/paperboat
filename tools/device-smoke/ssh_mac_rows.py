#!/usr/bin/env python3
"""F5-07..F5-13 managed-SSH checks with a Mac or Windows target and a Linux client (twins of tools/device-smoke/ssh_*.sh).

TARGET is the paired Mac (alias --alias, default apple, setup user --mac-user, default admin); CLIENT is a paired Linux
machine (a container) where `pb ssh <alias>` runs. Both are reached through wrappers that take ONE shell-string argument:
  MAC_CMD     runs it on the Mac (keychain unlocked); sudo there reads its password from $SUDO_PW_FILE (on the Mac)
  CLIENT_CMD  runs it on the client (needs passwordless sudo only for the outage step)
Usage: MAC_CMD=... CLIENT_CMD=... ssh_mac_rows.py [--only setup,outage,refusals,revoke,otheruser,audit,forwarding]
Steps with no safe Mac equivalent print SKIP <reason>. The Mac's sshd is only ever stopped by a detached root script
that restarts it on a timer; the pf block and the daemon SIGSTOP likewise have a timed failsafe.
--windows: TARGET is the paired Windows machine instead (ssh_windows_target.py; set SMOKE_WINDOWS_SSH and
PB_SMOKE_WINDOWS_PB, no MAC_CMD). otheruser uses a throwaway local Windows account with its own key and password.
Prints one JSON line; exit 1 on any FAIL.
"""
import argparse, json, os, re, secrets, shlex, subprocess, sys, time

ANCHOR = 'com.apple/pbsmoke'
SSHD_PLIST = '/System/Library/LaunchDaemons/ssh.plist'
ARGS = None
WIN = None  # ssh_windows_target.Target with --windows
RESULTS = []
# What a client runs ON the target through ssh; the Windows target has its own (cmd.exe is its shell).
REMOTE = {'NOP': 'true', 'HOLD_PB': 'echo session-up; sleep 300', 'HOLD_PLAIN': 'echo plain-up; sleep 300', 'HOLD_FWD': 'sleep 40',
          'SSHD_PORT': '22', 'AGENT_LIST': 'ssh-add -l', 'SHOW_DISPLAY': 'echo "display=<$DISPLAY>"'}


def run(env, script, timeout=300, input=None):
    cmd = shlex.split(os.environ[env]) + ['bash -c ' + shlex.quote(script)]
    try:
        p = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout,
                           **({'input': input} if input is not None else {'stdin': subprocess.DEVNULL}))
    except subprocess.TimeoutExpired:
        return 124, 'timeout after %ds' % timeout
    return p.returncode, p.stdout + p.stderr


def mac(script, timeout=120):
    return run('MAC_CMD', script, timeout)[1]


def daemon_pids():
    """The Mac daemon's pid to pause: launchd's job for its label, never a name match, which also
    finds the user's own `pb daemon ...` commands."""
    if WIN:
        return WIN.daemon_pids()
    match = re.search(r'^\s*pid = (\d+)$', mac('launchctl print gui/$(id -u)/com.pinksaucepasta.paperboatd'), re.MULTILINE)
    return [match.group(1)] if match else []


def cl(script, timeout=300, input=None):
    return run('CLIENT_CMD', script, timeout, input=input)[1]


def msudo(script, bg=False, timeout=120):
    # SUDO_PW_FILE is a path on the Mac, set here on the controller: expand it before the command leaves
    c = "sudo -S -p '' sh -c %s < %s" % (shlex.quote(script), shlex.quote(os.environ['SUDO_PW_FILE']))
    return mac(('nohup ' + c + ' >/dev/null 2>&1 &') if bg else c, timeout)


def prelude(wait_ssh=True):
    s = ''.join('%s=%s; ' % (k, shlex.quote(v)) for k, v in (WIN.remote() if WIN else REMOTE).items())
    s += ('PB=~/.local/bin/pb; A=%s; D=; for i in $(seq 1 60); do D=$(grep -o "Host \\*\\.[^ ]*" ~/.ssh/paperboat_config 2>/dev/null'
         ' | head -1 | sed "s/Host \\*\\.//"); [ -n "$D" ] && break; sleep 2; done; HN=$A.$D; H=%s@$HN; echo domain=$D; '
         # the client's unix user (pbt) is not an account on the Mac: plain ssh logs in as the Mac setup user
         % (shlex.quote(ARGS.alias), shlex.quote(ARGS.mac_user)))
    if wait_ssh:
        s += ('for i in $(seq 1 60); do $PB machine list 2>/dev/null | grep -q "$A " && break; sleep 2; done; '
              'for i in $(seq 1 30); do ssh -o BatchMode=yes $H $NOP </dev/null >/dev/null 2>&1 && break; sleep 2; done; ')
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
plain=; pb=; for i in 1 2 3 4 5 6 7; do a=$(ms ssh $H $NOP); b=$(ms $PB ssh $A -- $NOP); echo "RUN $i plain=$a pb=$b"; plain="$plain $a"; pb="$pb $b"; done
p50() { tr ' ' '\n' | grep -v '^$' | sort -n | sed -n 4p; }
echo "RESULT plain_p50=$(echo $plain | p50) pb_p50=$(echo $pb | p50) plain=[$plain ] pb=[$pb ]"''', 600)
    m = re.search(r'RESULT plain_p50=(-?\d+) pb_p50=(-?\d+) plain=\[([^\]]*)\] pb=\[([^\]]*)\]', out)
    ok = m and 0 <= int(m[1]) < 1000 and 0 <= int(m[2]) < 1000 and '-1' not in (m[3] + ' ' + m[4]).split()
    report('F5-07 setup', 'PASS' if ok else 'FAIL', m[0] if m else out[-300:].replace('\n', ' '))


def rcl(script, timeout=300):
    return run('REVOKE_CLIENT_CMD', script, timeout)[1]


def key_blob(call):
    return call("awk '{print $2}' ~/.ssh/paperboat-managed-ssh.pub").strip().splitlines()[-1:] or ['']


def listed(blob):
    """Is this managed key in the Mac's authorized_keys (the file sshd reads, asserted in outage())?"""
    if WIN:
        return WIN.listed(blob)
    return mac('grep -cF %s "$HOME/.ssh/authorized_keys"' % shlex.quote(blob)).strip().splitlines()[-1:] == ['1']


def outage():
    """F5-08: R (REVOKE_CLIENT_CMD, a fresh throwaway client) signs out first and the Mac's sshd stops listing its
    key; then the control plane is blackholed on the Mac and on both clients: the unrevoked client C (CLIENT_CMD)
    still opens plain ssh and `pb ssh` sessions, R is still refused, and the key lists agree before, during and after."""
    row = 'F5-08 outage'
    if not os.environ.get('REVOKE_CLIENT_CMD'):
        return report(row, 'SKIP', 'set REVOKE_CLIENT_CMD to a fresh throwaway client R (it signs out)')
    for who, call in (('C', cl), ('R', rcl)):
        if 'sudo-ok' not in call('sudo -n true && command -v ip >/dev/null && echo sudo-ok'):
            return report(row, 'SKIP', 'client %s has no passwordless sudo/ip to blackhole the control plane' % who)
    if not check_warm(row):
        return
    if WIN:
        if not WIN.keys_file():
            return report(row, 'FAIL', 'the managed sshd does not name exactly one Paperboat key file: %s' % WIN.sshd['authorized_keys'])
    else:
        sshd = msudo("sshd -T -C user=%s,host=localhost,addr=127.0.0.1 | grep -i '^authorizedkeysfile'" % ARGS.mac_user)
        if '.ssh/authorized_keys' not in sshd:
            return report(row, 'FAIL', "sshd does not read ~/.ssh/authorized_keys: " + sshd.strip()[-120:])
    base = rcl(prelude() + 'ssh $H echo warm-ok </dev/null 2>&1; $PB ssh $A -- echo warm-pb-ok </dev/null 2>&1')
    if 'warm-ok' not in base or 'warm-pb-ok' not in base:
        return report(row, 'FAIL', 'R had no session before it signed out: ' + base[-200:].replace('\n', ' '))
    kc, kr = key_blob(cl)[0], key_blob(rcl)[0]
    if not (kc and kr and kc != kr and listed(kc) and listed(kr)):
        return report(row, 'FAIL', 'both clients keys must be distinct and listed before the revoke')
    rcl(prelude(False) + '$PB logout >/dev/null 2>&1; echo signed-out')
    start, withdrawn = time.time(), None
    while time.time() - start < 90:
        if not listed(kr):
            withdrawn = round(time.time() - start, 1)
            break
        time.sleep(2)
    if withdrawn is None or not listed(kc):
        return report(row, 'FAIL', "after R signed out: withdrawn=%s, C's key listed=%s" % (withdrawn, listed(kc)))
    api = cl(r"""sed -n 's/.*"server_url": *"https:\/\/\([^"\/]*\).*/\1/p' ~/.config/paperboat/config.json""").strip()
    addrs = sorted(set(cl("getent ahosts %s | awk '{print $1}'" % shlex.quote(api)).split())) if api else []
    if not addrs:
        return report(row, 'FAIL', 'could not resolve the control plane address on the client')
    rule = 'block drop out quick proto { tcp udp } to { %s }' % ' '.join(addrs)
    token, c_out, r_out = None, '', ''
    probe = prelude(False) + r"""plain=$(timeout 40 ssh -o BatchMode=yes -o ConnectTimeout=20 $H echo outage-plain-ok </dev/null 2>&1 | tr '\r\n' '  '); pbo=$(timeout 40 $PB ssh $A -- echo outage-pb-ok </dev/null 2>&1 | tr '\r\n' '  ')
api=$(curl -s -m 5 -o /dev/null -w '%%{http_code}' https://%s/ || true); echo "RESULT api=$api plain=[$plain] pb=[$pbo]"
""" % api
    try:
        if not WIN:
            msudo('sleep 240; /sbin/pfctl -a %s -F all' % ANCHOR, bg=True)  # root failsafe on the Mac
        for call in (cl, rcl):
            for a in addrs:
                call('sudo -n ip route add blackhole %s 2>/dev/null || sudo -n ip -6 route add blackhole %s' % (a, a))
        if WIN:
            token = WIN.block(addrs)
        else:
            out = msudo('printf "%%s\\n" %s | /sbin/pfctl -a %s -f - 2>&1; /sbin/pfctl -E 2>&1; /sbin/pfctl -F states 2>&1' % (shlex.quote(rule), ANCHOR))
            t = re.search(r'Token : (\d+)', out)
            token = t[1] if t else None
        time.sleep(45)
        during = (listed(kc), listed(kr))
        c_out, r_out = cl(probe, 400), rcl(probe, 400)
    finally:
        if not WIN:
            msudo('/sbin/pfctl -a %s -F all%s' % (ANCHOR, '; /sbin/pfctl -X %s' % token if token else ''))
        elif token:
            WIN.unblock(token)
        for call in (cl, rcl):
            for a in addrs:
                call('sudo -n ip route del blackhole %s 2>/dev/null || sudo -n ip -6 route del blackhole %s' % (a, a))
    after = (listed(kc), listed(kr))
    c = ' '.join(l for l in c_out.splitlines() if l.startswith('RESULT'))
    r = ' '.join(l for l in r_out.splitlines() if l.startswith('RESULT'))
    ok = ('api=000' in c and re.search(r'plain=\[[^\]]*outage-plain-ok', c) and re.search(r'pb=\[[^\]]*outage-pb-ok', c)
          and 'api=000' in r and 'outage-plain-ok' not in r and 'outage-pb-ok' not in r
          and during == (True, False) and after == (True, False))
    report(row, 'PASS' if ok else 'FAIL', 'withdrawn_after=%ss keys(C,R) during=%s after=%s | C %s | R %s' % (withdrawn, during, after, c or c_out[-200:], r or r_out[-200:]))


def wait_sshd():
    for _ in range(30):
        if WIN.sshd_running() if WIN else 'up' in mac('echo up'):
            return True
        time.sleep(5)
    return False


def case(name):
    return cl(prelude(False) + 'echo "CASE %s [$(timeout 90 ssh $H $NOP </dev/null 2>&1 | tr "\\n" " " | cut -c1-300)]"' % name, 150)


def refusals():
    row = 'F5-09 refusals'
    if not check_warm(row):
        return
    res = ''
    try:  # one detached root script stops sshd, waits, and starts it again, whatever happens to us
        if WIN:
            WIN.stop_sshd()
        else:
            msudo('launchctl bootout system/com.openssh.sshd; sleep 40; launchctl bootstrap system %s' % SSHD_PLIST, bg=True)
        time.sleep(4)
        res += case('sshd_stopped')
    finally:
        back = wait_sshd()
    if not back:
        return report(row, 'FAIL', 'sshd did not come back; the root timer should have restarted it')
    time.sleep(5)
    pids = daemon_pids()
    if pids and WIN:  # the same on Windows: every Paperboat service process suspended, resumed on a timer
        try:
            WIN.pause(pids)
            res += case('machine_offline')
        finally:
            WIN.resume(pids)
    elif pids:  # SIGSTOP the Mac's daemon (docker pause equivalent), SIGCONT on a timer
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


# F5-10 cli mode on a Mac target (twin of ssh_live_revoke.sh): the client holds a `pb ssh` AND a plain managed ssh session
# to the Mac, `pb logout` revokes its CLI session; both must end within 15 s and both must say access was revoked; a
# normal exit of either prints nothing. The clock starts before the logout.
REVOKE = r'''# normal exits print nothing
perr=$(timeout 60 $PB ssh $A -- 'exit 0' </dev/null 2>&1 >/dev/null | tr '\n' ' '); lerr=$(timeout 60 ssh $H 'exit 0' </dev/null 2>&1 >/dev/null | tr '\n' ' ')
echo "RESULT normal pb_stderr=[$perr] plain_stderr=[$lerr]"
$PB ssh $A -- "$HOLD_PB" > ~/slr.out 2> ~/slr.err < /dev/null & pid=$!
ssh $H "$HOLD_PLAIN" > ~/slrp.out 2> ~/slrp.err < /dev/null & ppid=$!
for i in $(seq 1 60); do grep -q session-up ~/slr.out && grep -q plain-up ~/slrp.out && break; sleep 1; done
grep -q session-up ~/slr.out && grep -q plain-up ~/slrp.out || { echo "RESULT no-session pb=$(tr '\n' ' ' < ~/slr.err | cut -c1-160) plain=$(tr '\n' ' ' < ~/slrp.err | cut -c1-160)"; exit 0; }
start=$(date +%s.%N); $PB logout >/dev/null 2>&1
for i in $(seq 1 300); do kill -0 $pid 2>/dev/null || break; sleep 0.1; done
end=$(date +%s.%N)
for i in $(seq 1 100); do kill -0 $ppid 2>/dev/null || break; sleep 0.1; done
# a plain session still open 10 s after the revoke: record what the daemon was doing (relay or direct, which steps) while it hangs
kill -0 $ppid 2>/dev/null && echo "RESULT hang-diag [$(cat ~/.local/state/paperboat/diagnostics/*.ndjson 2>/dev/null | tail -400 | grep -E 'reconnect_transport|redial_step|peer_stream|relay|native_private' | tail -16 | python3 -c "import sys,json
for l in sys.stdin:
    d=json.loads(l); f=d.get('fields',{}); print(d['at'][11:23], d['code'], f.get('step'), f.get('state'), f.get('path_category'), f.get('cause'), end='; ')" | cut -c1-1600)]"
for i in $(seq 1 200); do kill -0 $ppid 2>/dev/null || break; sleep 0.1; done
pend=$(date +%s.%N)
palive=$(kill -0 $ppid 2>/dev/null && echo yes || echo no); alive=$(kill -0 $pid 2>/dev/null && echo yes || echo no); kill $pid $ppid 2>/dev/null
echo "RESULT seconds=$(python3 -c "print(round($end-$start,1))") alive=$alive plain_seconds=$(python3 -c "print(round($pend-$start,1))") plain_alive=$palive stderr=[$(tr '\n' ' ' < ~/slr.err | cut -c1-200)] plain_stderr=[$(tr '\n' ' ' < ~/slrp.err | cut -c1-200)]"'''


def revoke_verdict(out):
    """(ok, why) for the REVOKE script's output: both sessions end within 15 s, both say access was revoked, normal exits are silent."""
    out = re.sub(r'\s+', ' ', out)  # ssh error text carries line breaks
    m = re.search(r'RESULT seconds=([0-9.]+) alive=(\w+) plain_seconds=([0-9.]+) plain_alive=(\w+) stderr=\[(.*?)\] plain_stderr=\[(.*)\]', out)
    if not m:
        return False, 'no measurement'
    revoked = re.compile(r'access (to \S+ )?was revoked', re.I)
    if not (float(m[1]) <= 15 and float(m[3]) <= 15 and m[2] == 'no' and m[4] == 'no'):
        return False, 'a session outlived the revoke'
    if not (revoked.search(m[5]) and revoked.search(m[6])):
        return False, 'a session did not say access was revoked'
    if 'RESULT normal pb_stderr=[] plain_stderr=[]' not in out:
        return False, 'a normal exit printed something'
    return True, ''


def revoke():
    # `pb logout` signs the client out for good, so it only runs on a FRESH throwaway client (REVOKE_CLIENT_CMD).
    if not os.environ.get('REVOKE_CLIENT_CMD'):
        report('F5-10 revoke', 'SKIP', 'set REVOKE_CLIENT_CMD to a fresh throwaway client; logout would sign out REVOKE_CLIENT_CMD')
        return
    _, out = run('REVOKE_CLIENT_CMD', prelude() + REVOKE, timeout=300)
    ok, why = revoke_verdict(out)
    report('F5-10 revoke', 'PASS' if ok else 'FAIL', ('' if ok else why + ': ') + re.sub(r'\s+', ' ', out)[-2400:])


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
    bob = ('w6ssh' if WIN else 'pbbob') + secrets.token_hex(3)
    pw = secrets.token_urlsafe(24) + 'Aa1!' if WIN else secrets.token_hex(10)
    out, sid, cleanup = '', None, ''
    try:
        pub = cl('umask 077; rm -f ~/bob_key ~/bob_key.pub; ssh-keygen -q -t ed25519 -N "" -f ~/bob_key && cat ~/bob_key.pub && rm -f ~/bob_key.pub').strip()
        if not pub.startswith('ssh-ed25519'):
            return report(row, 'FAIL', "could not make bob's key on the client")
        cl('umask 077; cat > ~/bob_pw', input=pw)
        if WIN:
            sid = WIN.create_other_user(bob, pw, pub)
        else:
            msudo('sysadminctl -addUser {b} -fullName {b} -password {p} -home /Users/{b} && (createhomedir -c -u {b} >/dev/null 2>&1; mkdir -p /Users/{b}; chown {b}:staff /Users/{b}; chmod 750 /Users/{b}); '
              '(dscl . -read /Groups/com.apple.access_ssh >/dev/null 2>&1 && dseditgroup -o edit -a {b} -t user com.apple.access_ssh); '
              'install -d -m 700 -o {b} /Users/{b}/.ssh && printf "%s\\n" {k} > /Users/{b}/.ssh/authorized_keys '
              '&& chown {b} /Users/{b}/.ssh/authorized_keys && chmod 600 /Users/{b}/.ssh/authorized_keys'.format(b=bob, p=pw, k=shlex.quote(pub)))
        out = cl(prelude() + OTHER_USER % (bob, ARGS.mac_user), 600)
        if WIN:
            # The same key worked for the setup user above. Force its agent and
            # public selector so a client-side user filter cannot hide a server bug.
            out += cl(prelude(False) + 'B=' + shlex.quote(bob) + r''';
agent=$(ssh -G "$H" 2>/dev/null | sed -n 's/^identityagent //p' | head -1)
key=$(ssh -G "$H" 2>/dev/null | sed -n 's/^identityfile //p' | grep 'paperboat-managed-ssh.pub$' | head -1)
if [ -z "$agent" ] || [ -z "$key" ]; then echo MANAGED_OTHER_USER=missing-identity; exit 1; fi
o=$(ssh -v -o BatchMode=yes -o IdentitiesOnly=yes -o "IdentityAgent=$agent" -i "$key" "$B@$HN" whoami 2>&1 </dev/null); rc=$?
if [ "$rc" != 0 ] && grep -q 'Permission denied' <<<"$o" && grep -q 'Offering public key' <<<"$o" && ! grep -q 'agent refused operation' <<<"$o"; then
echo MANAGED_OTHER_USER=refused
else echo MANAGED_OTHER_USER=not-refused; fi
''')
    finally:
        cl('rm -f ~/bob_key ~/bob_pw')
        if WIN:
            if sid:
                try:  # a cleanup failure is reported next to the verdict, never instead of it
                    WIN.remove_other_user(sid)
                except Exception as e:
                    cleanup = ' cleanup failed: %r' % e
        else:
            msudo('sysadminctl -deleteUser {b} >/dev/null 2>&1; dscl . -delete /Users/{b} 2>/dev/null; rm -rf /Users/{b}'.format(b=bob))
    m = re.search(r'RESULT ok:\[([^\]]*)\] fail:\[([^\]]*)\] skip:\[([^\]]*)\]', out)
    need = {'setup-user-pb', 'setup-user-plain', 'bob-key-plain', 'bob-agent-pb', 'bob-agent-plain'}
    if m and not m[3].strip():
        need |= {'bob-password-plain', 'bob-password-pb', 'bob-wrong-password-is-sshd'}
    ok = m and not m[2].strip() and need <= set(m[1].split())
    if WIN:
        # sshd -T -C on the target: Paperboat's key file goes to domain\user exactly (ssh_windows_target.py)
        ok = ok and 'MANAGED_OTHER_USER=refused' in out and WIN.sshd.get('domain_scope') == 'exact'
    if m and m[3].strip():
        report(row + ' password', 'SKIP', m[3].strip())
    detail = m[0] if m else out[-300:].replace('\n', ' ')
    if WIN:
        detail += ' domain_scope=%s%s' % (WIN.sshd.get('domain_scope'), cleanup)
    report(row, 'PASS' if ok else 'FAIL', detail)


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
             "AND created_at > now() - interval '%d seconds' ORDER BY created_at, id;" % (mid, int(time.time() - began) + 90))
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


OFFLINE_OUTAGE_SECS = 150  # longer than the signed network's 2 min refresh hint (tailnet/authority.go OutOfSync)


def target_block_control_plane(addrs, seconds=OFFLINE_OUTAGE_SECS + 90):
    """Block the target's control plane until target_unblock_control_plane() or `seconds` on the device.

    Windows: a firewall rule dropping all outbound to those addresses (ssh_windows_target.Target.block,
    the same call outage() uses); the rule is address-scoped, so the controller's SSH stays up. macOS:
    the same pf anchor rule outage() uses. The timed undo is armed before the block lands, so the
    outage cannot survive this controller on any exit path. Small target-specific functions so the
    macOS target can reuse offline_audit() later."""
    if WIN:
        return WIN.block(addrs, seconds=seconds)
    rule = 'block drop out quick proto { tcp udp } to { %s }' % ' '.join(addrs)
    msudo('sleep %d; /sbin/pfctl -a %s -F all' % (seconds, ANCHOR), bg=True)  # root failsafe on the Mac
    out = msudo('printf "%%s\\n" %s | /sbin/pfctl -a %s -f - 2>&1; /sbin/pfctl -E 2>&1; /sbin/pfctl -F states 2>&1'
                % (shlex.quote(rule), ANCHOR))
    t = re.search(r'Token : (\d+)', out)
    return t[1] if t else None


def target_unblock_control_plane(token):
    """Undo target_block_control_plane() on every exit path (the device-side timer is the backstop)."""
    if WIN:
        if token:
            WIN.unblock(token)
        return
    msudo('/sbin/pfctl -a %s -F all%s' % (ANCHOR, '; /sbin/pfctl -X %s' % token if token else ''))


def offline_audit():
    """F5-12 offline half (twin of tools/device-smoke/ssh_offline_audit.sh): the host records origin 'offline'
    for a managed SSH session only when the client sent the offline header (pb ssh always does) and its
    signed network config is past its refresh hint (OutOfSync, 2 min). The control plane is therefore
    blocked on the TARGET for OFFLINE_OUTAGE_SECS, one real `pb ssh <alias> -- echo <marker>` runs in that
    window, the block is lifted, and the host's upload (30 s cadence) must file machine.access_started and
    machine.access_ended with origin offline, the end naming the start's session id, outcome closed and
    bytes_in/out > 0. The client is left alone: only the host's OutOfSync decides the origin."""
    row = 'F5-12 offline-audit'
    if not check_warm(row):
        return
    try:
        mid = find_machine_id(json.loads(cl('~/.local/bin/pb machine list --json 2>/dev/null')))
    except ValueError:
        mid = None
    if not mid:
        return report(row, 'SKIP', 'could not read the target machine id from `pb machine list --json` on the client')
    api = cl(r"""sed -n 's/.*"server_url": *"https:\/\/\([^"\/]*\).*/\1/p' ~/.config/paperboat/config.json""").strip()
    addrs = sorted(set(cl("getent ahosts %s | awk '{print $1}'" % shlex.quote(api)).split())) if api else []
    if not addrs:
        return report(row, 'FAIL', 'could not resolve the control plane address on the client')
    began = time.time()
    base_out = cl('~/.local/bin/pb ssh %s -- echo ssh-offline-baseline-%d </dev/null 2>&1' % (shlex.quote(ARGS.alias), began), 300)
    if 'ssh-offline-baseline-%d' % began not in base_out:
        return report(row, 'FAIL', 'pb ssh did not run before the outage: %s' % base_out[-200:].replace('\n', ' '))
    marker = 'ssh-offline-%d' % began
    token, blackholed_at, off_out = None, None, ''
    try:
        token = target_block_control_plane(addrs)
        blackholed_at = time.time()
        # While still blocked: the client always sends the offline header, the host records origin offline
        # once it is past its refresh hint (OutOfSync, 2 min). Dial from early in the outage and accept the first
        # success past the hint: relayed dials flap (direct one minute, regional_relay the next), so one wobble
        # must not fail the check. An early success is online and ignored by the verdict; only an offline-origin
        # pair in the window passes. The block's device-side timer (OFFLINE_OUTAGE_SECS + 90) covers the loop.
        deadline = blackholed_at + OFFLINE_OUTAGE_SECS + 80
        ever = False
        while True:
            off_out = cl('~/.local/bin/pb ssh %s -- echo %s </dev/null 2>&1' % (shlex.quote(ARGS.alias), marker), 90)
            ever = ever or marker in off_out
            el = time.time() - blackholed_at
            if (marker in off_out and el >= OFFLINE_OUTAGE_SECS) or time.time() >= deadline:
                break
            time.sleep(20)
        if not ever:
            return report(row, 'FAIL', 'pb ssh did not run during the %ds outage: %s'
                          % (OFFLINE_OUTAGE_SECS, off_out[-200:].replace('\n', ' ')))
    finally:
        target_unblock_control_plane(token)  # also runs when the session above failed
    reconnected_at = time.time()
    db = os.environ.get('PB_AUDIT_DB_HOST', 'root@coolify')
    # Scope the read to this run: sessions start at or after the block went up (the baseline is online and
    # filtered by origin anyway). A began-anchored window would re-read a previous run's offline rows when runs
    # follow within minutes and PASS on stale data.
    query = ("SELECT event_type, metadata->>'origin', metadata->>'session_id', metadata->>'outcome', coalesce(metadata->>'bytes_in','0'), "
             "coalesce(metadata->>'bytes_out','0'), metadata->>'peer_endpoint_id', idempotency_key FROM paperboat.audit_events WHERE resource_id='%s' "
             "AND event_type IN ('machine.access_started','machine.access_ended') AND metadata->>'capability'='managed_ssh' "
             "AND created_at > now() - interval '%d seconds' ORDER BY created_at, id;" % (mid, int(time.time() - blackholed_at) + 30))
    rows = ''
    for _ in range(30):  # the host uploads within its 30 s cadence
        rows = subprocess.run(['ssh', db, 'docker exec -i paperboat-postgres-1 psql -U postgres -d paperboat -qAt -F "|"'], input=query,
                              capture_output=True, text=True).stdout
        if re.search(r'^machine.access_ended\|offline\|', rows, re.M):
            break
        time.sleep(5)
    recs = [r.split('|') for r in rows.splitlines() if r]
    # An end names its start: the start's host event id, which the server files as idempotency key "offline:<id>".
    off = [r for r in recs if r[1] == 'offline']
    starts = [r for r in off if r[0] == 'machine.access_started']
    ends = [r for r in off if r[0] == 'machine.access_ended']
    start_ids = {s[7].removeprefix('offline:') for s in starts}
    ok = bool(starts) and bool(ends)
    for e in ends:
        ok = ok and e[3] == 'closed' and int(e[4]) > 0 and int(e[5]) > 0 and e[2] in start_ids
    for s in starts:
        ok = ok and s[6].startswith('session_')
    wall = int(reconnected_at - blackholed_at) if blackholed_at else OFFLINE_OUTAGE_SECS
    detail = 'outage %ss ' % wall + ' '.join('%s:%s:%s:%s:%s' % (r[0][8:], r[2][:8], r[3], r[4], r[5]) for r in off)
    report(row, 'PASS' if ok else 'FAIL', detail[:600])


FORWARDING = r'''fail=; ok=
check() { n=$1; w=$2; shift 2; o=$("$@" 2>&1 </dev/null); if grep -q -- "$w" <<<"$o"; then ok="$ok $n"; else fail="$fail $n($(tr '\n' ' ' <<<"$o" | cut -c1-160))"; fi; }
banner() { for i in $(seq 1 30); do timeout 5 bash -c "exec 3<>/dev/tcp/127.0.0.1/$1; head -c 7 <&3" 2>/dev/null && return; sleep 1; done; echo "no listener on $1"; }
ssh -o ExitOnForwardFailure=yes -L 127.0.0.1:12222:127.0.0.1:$SSHD_PORT $H "$HOLD_FWD" </dev/null >/dev/null 2>&1 &
check port-forward-plain SSH-2.0 banner 12222
kill %1 2>/dev/null; wait
check agent-forward-plain ED25519 ssh -A $H "$AGENT_LIST"
check no-x11-plain "display=<>" env DISPLAY=:0 ssh $H "$SHOW_DISPLAY"
check no-x11-pb "display=<>" env DISPLAY=:0 $PB ssh $A -- "$SHOW_DISPLAY"
echo "RESULT ok:[$ok ] fail:[$fail ]"'''


# Agent forwarding INTO a Windows machine is a documented limit (Win32-OpenSSH #1461, #1865: Microsoft's sshd hardcodes /tmp/ssh-XXX and
# refuses the AF_UNIX server bind; spark/w6-f513-limit-proof.md). On a Windows target `pb ssh -A` must say so, in exactly this line
# (cli/cmd/pb/ssh_command.go sshWindowsAgentForwardingMessage), and the session must go on. Port forwarding and X11-off stay exact.
WINDOWS_AGENT_LIMIT = "Agent forwarding isn't available on Windows machines (Microsoft OpenSSH limitation); port forwarding works."
WINDOWS_AGENT_CHECKS = (
    'check agent-limit-message "$WINDOWS_AGENT_LIMIT" $PB ssh $A -- -A "echo agent-session-ran"\n'
    'check agent-session-continues agent-session-ran $PB ssh $A -- -A "echo agent-session-ran"\n'
    'check agent-plain-session-continues agent-session-ran ssh -A $H "echo agent-session-ran"\n')


def forwarding_script(windows):
    old = 'check agent-forward-plain ED25519 ssh -A $H "$AGENT_LIST"\n'
    assert old in FORWARDING + '\n'
    if not windows:
        return FORWARDING
    return ('WINDOWS_AGENT_LIMIT=%s\n' % shlex.quote(WINDOWS_AGENT_LIMIT)) + FORWARDING.replace(old, WINDOWS_AGENT_CHECKS)


def forwarding():
    row = 'F5-13 forwarding'
    if not check_warm(row):
        return
    out = cl(prelude() + forwarding_script(WIN is not None), 600)
    want = 'RESULT ok:[ port-forward-plain agent-forward-plain no-x11-plain no-x11-pb ] fail:[ ]'
    if WIN is not None:
        want = 'RESULT ok:[ port-forward-plain agent-limit-message agent-session-continues agent-plain-session-continues no-x11-plain no-x11-pb ] fail:[ ]'
    report(row, 'PASS' if want in out else 'FAIL', ' '.join(l for l in out.splitlines() if l.startswith('RESULT')) or out[-300:].replace('\n', ' '))


STEPS = {'setup': setup, 'outage': outage, 'refusals': refusals, 'revoke': revoke, 'otheruser': otheruser, 'audit': audit, 'offline_audit': offline_audit, 'forwarding': forwarding}


def main():
    global ARGS, WIN
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument('--only', default=','.join(STEPS))
    p.add_argument('--alias', default='apple')
    p.add_argument('--mac-user', '--user', help='the setup user on the target (default: admin; with --windows, the login user there)')
    p.add_argument('--windows', action='store_true')
    ARGS = p.parse_args()
    if ARGS.windows:
        import ssh_windows_target
        WIN = ssh_windows_target.Target()
    ARGS.mac_user = ARGS.mac_user or (WIN.user() if WIN else 'admin')
    names = ARGS.only.split(',')
    unknown = [n for n in names if n not in STEPS]
    if unknown or not all(os.environ.get(v) for v in ('CLIENT_CMD',) + (() if WIN else ('MAC_CMD',))):
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
