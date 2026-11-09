#!/usr/bin/env python3
"""F5-10 grant|team on a Mac target (twin of ssh_live_revoke.sh grant|team): a team MEMBER's open ssh sessions to the Mac end
within 15 s of the owner withdrawing the grant (grant) or removing the member (team), and pb says access was revoked.

Usage: MODE=grant|team ssh_mac_member_revoke.py APPLE_MACHINE_ID
Env (wrappers take ONE shell-string argument, as in ssh_mac_rows.py):
  CLIENT_CMD         runs it on the OWNER's signed-in client (the Mac's account A), e.g. the macl-c container
  REVOKE_CLIENT_CMD  runs it on a FRESH container with pb installed and nobody signed in: it becomes the member B
  MAC_CMD            runs it on the Mac (not used for sshd here, only for logs)
  TEAM               a team A owns; MEMBER_STATE  B's console storage state; PW_CHROMIUM, PW_MODULE (playwright)
  MAC_USER (default admin), ALIAS (default apple)
Steps: B signs in (approved in the browser), A invites B, B accepts, A shares the Mac into TEAM and grants B managed_ssh+exec;
gate: right after the grant, with B never having run exec, `ssh admin@apple.<domain> echo ok` and `pb --workspace TEAM ssh apple`
print ok within 5 s, B's managed config has the Mac's block and its ssh_known_hosts has its lines; then B holds a `pb ssh` AND a
plain ssh session, A revokes, both must end within 15 s saying access was revoked; normal exits print nothing.
Prints one JSON line; the share and the membership are undone on every exit path.
"""
import json
import os
import re
import shlex
import subprocess
import sys
import time

import ssh_mac_rows as m

HERE = os.path.dirname(os.path.abspath(__file__))
MODE = os.environ.get('MODE', 'grant')
ALIAS = os.environ.get('ALIAS', 'apple')
USER = os.environ.get('MAC_USER', 'admin')
TEAM = os.environ['TEAM']

PROLOGUE = ('PB=~/.local/bin/pb; A=%s; D=; for i in $(seq 1 60); do D=$(grep -o "Host \\*\\.[^ ]*" ~/.ssh/paperboat_config 2>/dev/null | head -1 | sed "s/Host \\*\\.//"); '
            '[ -n "$D" ] && break; sleep 2; done; N=$A.$D; H=%s@$N; PBW="--workspace %s"; ' % (ALIAS, USER, TEAM))

ACCESS = PROLOGUE + r'''t0=$(date +%s.%N); okp=; okpl=
while [ -z "$okp" ] || [ -z "$okpl" ]; do
  [ -n "$okpl" ] || { lerr=$(ssh -o ConnectTimeout=4 $H echo ok </dev/null 2>&1 | tr '\n' ' '); grep -q '^ok ' <<<"$lerr" && okpl=$(date +%s.%N); }
  [ -n "$okp" ] || { perr=$(timeout 20 $PB $PBW ssh $A -- echo ok </dev/null 2>&1 | tr '\n' ' '); grep -q '^ok ' <<<"$perr" && okp=$(date +%s.%N); }
  [ "$(python3 -c "import time;print(int(time.time()-$t0>40))")" = 0 ] || break; sleep 1
done
block=$(grep -c "^Host $N\$" ~/.ssh/paperboat_config); kh=$(grep -c "^$N " /run/user/$(id -u)/paperboat/ssh_known_hosts 2>/dev/null)
echo "RESULT access plain_s=$([ -n "$okpl" ] && python3 -c "print(round($okpl-$t0,1))" || echo none) pb_s=$([ -n "$okp" ] && python3 -c "print(round($okp-$t0,1))" || echo none) config_block=$block known_hosts_lines=$kh last_plain=[$([ -n "$okpl" ] || cut -c1-200 <<<"$lerr")] last_pb=[$([ -n "$okp" ] || cut -c1-200 <<<"$perr")]"'''

HOLD = PROLOGUE + r'''perr=$($PB $PBW ssh $A -- 'exit 0' </dev/null 2>&1 >/dev/null | tr '\n' ' '); lerr=$(ssh $H 'exit 0' </dev/null 2>&1 >/dev/null | tr '\n' ' ')
echo "RESULT normal pb_stderr=[$perr] plain_stderr=[$lerr]"
$PB $PBW ssh $A -- 'echo session-up; sleep 300' > ~/slr.out 2> ~/slr.err < /dev/null & pid=$!
ssh $H 'echo plain-up; sleep 300' > ~/slrp.out 2> ~/slrp.err < /dev/null & ppid=$!
for i in $(seq 1 60); do grep -q session-up ~/slr.out && grep -q plain-up ~/slrp.out && break; sleep 1; done
grep -q session-up ~/slr.out && grep -q plain-up ~/slrp.out || { echo "RESULT no-session pb=$(tr '\n' ' ' < ~/slr.err | cut -c1-160) plain=$(tr '\n' ' ' < ~/slrp.err | cut -c1-160)"; exit 0; }
rm -f ~/go; echo HOLDING
for i in $(seq 1 600); do [ -e ~/go ] && break; sleep 0.2; done
start=$(date +%s.%N)
for i in $(seq 1 300); do kill -0 $pid 2>/dev/null || break; sleep 0.1; done
end=$(date +%s.%N)
for i in $(seq 1 300); do kill -0 $ppid 2>/dev/null || break; sleep 0.1; done
pend=$(date +%s.%N)
palive=$(kill -0 $ppid 2>/dev/null && echo yes || echo no); alive=$(kill -0 $pid 2>/dev/null && echo yes || echo no); kill $pid $ppid 2>/dev/null
echo "RESULT seconds=$(python3 -c "print(round($end-$start,1))") alive=$alive plain_seconds=$(python3 -c "print(round($pend-$start,1))") plain_alive=$palive stderr=[$(tr '\n' ' ' < ~/slr.err | cut -c1-200)] plain_stderr=[$(tr '\n' ' ' < ~/slrp.err | cut -c1-200)]"'''


def owner(script, timeout=120):
    return m.run('CLIENT_CMD', 'PB=~/.local/bin/pb; TEAM=%s; gen() { $PB team get "$TEAM" --json | python3 -c "import json,sys;print(json.load(sys.stdin)[\\"generation\\"])"; }; %s'
                 % (shlex.quote(TEAM), script), timeout)


def member(script, timeout=120):
    return m.run('REVOKE_CLIENT_CMD', script, timeout)


def sign_in_member():
    """B signs in with the browser approval; returns (account id, email)."""
    member("nohup ~/.local/bin/pb login --no-browser --json > ~/login.out 2>&1 < /dev/null & sleep 1")
    url = ''
    for _ in range(30):
        found = re.search(r'https://[^"]*authorize\?code=[A-Z0-9-]*', member('cat ~/login.out')[1])
        if found:
            url = found[0]
            break
        time.sleep(2)
    if not url:
        raise RuntimeError('the member client printed no approval link')
    env = dict(os.environ, STATE=os.environ['MEMBER_STATE'], PW_MODULE=os.environ.get('PW_MODULE', 'playwright-core'))
    if subprocess.run(['node', os.path.join(HERE, 'approve_cli_login.mjs'), url], env=env, capture_output=True, timeout=180).returncode:
        raise RuntimeError("the member's login approval failed")
    for _ in range(30):
        out = member('~/.local/bin/pb auth status --json 2>/dev/null')[1]
        try:
            account = json.loads(out[out.index('{'):])['account']
            return account['id'], account['email']
        except (ValueError, KeyError):
            time.sleep(2)
    raise RuntimeError('the member client did not sign in')


def main():
    if MODE not in ('grant', 'team') or len(sys.argv) != 2:
        raise SystemExit('usage: MODE=grant|team ssh_mac_member_revoke.py APPLE_MACHINE_ID')
    tid, bid, status, detail = sys.argv[1], '', 'FAIL', ''
    try:
        bid, email = sign_in_member()
        inv = owner('$PB team invite "$TEAM" %s --generation $(gen) --json' % shlex.quote(email))[1]
        inv = json.loads(inv[inv.index('{'):])['invitation_id']
        member('~/.local/bin/pb team accept %s --json >/dev/null' % shlex.quote(inv))
        owner('$PB team machine share "$TEAM" %s --generation $(gen) --json >/dev/null' % shlex.quote(tid))
        owner('$PB team machine grant "$TEAM" %s --generation $(gen) --member %s --capability managed_ssh --capability exec --json >/dev/null' % (shlex.quote(tid), shlex.quote(bid)))
        access = ' '.join(l for l in member(ACCESS, 120)[1].splitlines() if l.startswith('RESULT access'))
        p, q = re.search(r'plain_s=(\S+)', access), re.search(r'pb_s=(\S+)', access)
        if not (p and q and 'none' not in (p[1], q[1]) and float(p[1]) <= 5 and float(q[1]) <= 5 and 'config_block=1' in access
                and not re.search(r'known_hosts_lines=0\b', access)):
            raise RuntimeError('member access gate: ' + access)
        detail = access
        # the hold script runs in the background on B; A revokes once it prints HOLDING
        member("cat > ~/hold.sh <<'EOS'\n%s\nEOS" % HOLD)
        member('nohup bash ~/hold.sh > ~/hold.out 2>&1 < /dev/null &')
        for _ in range(120):
            if 'HOLDING' in member('cat ~/hold.out')[1] or 'RESULT no-session' in member('cat ~/hold.out')[1]:
                break
            time.sleep(2)
        if 'HOLDING' not in member('cat ~/hold.out')[1]:
            raise RuntimeError('no sessions: ' + member('tail -c 300 ~/hold.out')[1].replace('\n', ' '))
        if MODE == 'grant':
            owner('$PB team machine grant "$TEAM" %s --generation $(gen) --member %s --capability managed_ssh --capability exec --active=false --json >/dev/null' % (shlex.quote(tid), shlex.quote(bid)))
        else:
            owner('$PB team remove "$TEAM" %s --generation $(gen) --json >/dev/null' % shlex.quote(bid))
            bid = ''
        member('touch ~/go')
        for _ in range(60):
            if 'RESULT seconds' in member('cat ~/hold.out')[1]:
                break
            time.sleep(2)
        res = member('cat ~/hold.out')[1]
        ok, why = m.revoke_verdict(res)
        status, detail = ('PASS' if ok else 'FAIL'), '%s %s %s' % (detail, '' if ok else why + ':', ' | '.join(l for l in res.splitlines() if l.startswith('RESULT'))[-700:])
    except Exception as error:  # every failure names its step
        detail = '%s %s' % (detail, error)
    finally:
        owner('$PB team machine unshare "$TEAM" %s --generation $(gen) --json >/dev/null 2>&1; [ -z %s ] || $PB team remove "$TEAM" %s --generation $(gen) --json >/dev/null 2>&1' % (shlex.quote(tid), shlex.quote(bid), shlex.quote(bid)))
    print(json.dumps({'check': 'ssh-mac-member-revoke', 'mode': MODE, 'status': status, 'detail': detail.strip()}))
    sys.exit(0 if status == 'PASS' else 1)


if __name__ == '__main__':
    main()
