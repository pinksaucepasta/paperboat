#!/usr/bin/env python3
"""exec (5 s) and ssh (10 s) to this machine itself and to a peer must work after a daemon restart.

Every leg (exec self, exec peer, ssh self, ssh peer) starts from its OWN cold daemon: `pb service restart` returns once the
daemon is back, then that leg's `pb exec` / `pb ssh` must print a product the REMOTE side computed (the arithmetic is in the
command's argument, never its text) within its limit, timed inside the machine that runs it. Every leg runs and all
failures are reported together: the Windows exec-to-self wedge and the Mac readiness flap after a restart both hid behind a
passing first command, and only the first leg of one restart is ever cold.

Limits: exec 5 s, ssh 10 s. Measured cold after a restart (p50/max, d0b4dc735): Linux 3.3/3.7 s, Windows 3.8/4.0 s, Mac
6.6/7.2 s; warm 1.4-2.1 s. ssh adds the key exchange and a login shell, so 5 s fits no OS for ssh and 10 s does.
Pool Macs share the image's SSH host keys: regenerate them before installing, or ssh legs fail on a host-key mismatch."""
import argparse
import json
import random
import subprocess
import time

from collect import run_all
from install_lifecycle import registration
from pairing_lifecycle import invoke
import windows_target
import winhost

DEADLINE = {'exec': 5.0, 'ssh': 10.0}  # seconds per leg, see the module docstring


def probe(binary, target, command='exec'):
    """(argv tail, expected stdout line): a sum no argument contains, computed by the remote shell. `pb ssh` joins its
    arguments with spaces for the remote login shell, so there the command is ONE argument (`sh -c a b` would run `a`)."""
    x, y = random.randint(1000, 9999), random.randint(1000, 9999)
    if windows_target.is_windows(binary, target):
        return ['cmd', '/c', 'set', '/a', '%d+%d' % (x, y)], str(x + y)
    if command == 'ssh':
        return ['echo $((%d+%d))' % (x, y)], str(x + y)
    return ['sh', '-c', 'echo $((%d+%d))' % (x, y)], str(x + y)


def verdict(elapsed, returncode, output, expected, deadline=DEADLINE['exec']):
    """PASS iff it succeeded in time and the remote-computed line is alone on a line of the output."""
    if elapsed > deadline:
        return False, 'took %.1fs, over the %.0fs deadline' % (elapsed, deadline)
    if returncode != 0:
        return False, 'exit %s' % returncode
    if expected not in output.replace('\r', '').split('\n'):
        return False, 'the remote answer %s was not in the output' % expected
    return True, ''


def attempt(binary, command, target, argv, expected, started, now=time.monotonic, deadline=None):
    """Retry `pb <command> <target> -- argv` until the deadline from `started`; the last failure names why."""
    deadline = deadline or DEADLINE[command]
    reason = 'no attempt'
    while True:
        remaining = deadline - (now() - started)
        if remaining <= 0:
            raise RuntimeError('%s %s: still failing %.0fs after the restart: %s' % (command, target, deadline, reason))
        try:
            result = winhost.run_pb(binary, [command, target, '--', *argv], max(1, remaining))
            ok, reason = verdict(now() - started, result.returncode, result.stdout.decode(errors='replace'), expected, deadline)
            if ok:
                return
            reason += ': ' + result.stderr.decode(errors='replace').strip()  # the whole stderr: a clipped tail hid why ssh failed
        except subprocess.TimeoutExpired:
            reason = 'no answer within %.0fs' % remaining
        time.sleep(0.25)


def cold(binary, command, target, argv, expected):
    """One leg from its own daemon restart; the 5 s start the moment the restart returns."""
    restart_local(binary)
    attempt(binary, command, target, argv, expected, time.monotonic())


def restart_local(binary):
    result = winhost.run_pb(binary, ['service', 'restart'], 120)
    if result.returncode:
        raise RuntimeError('pb service restart failed: ' + result.stderr.decode(errors='replace')[-200:])


# Windows client: ssh + sshpass + powershell cost about 3.6 s a call (measured), most of the 5 s. So every leg's restart and the
# leg run back to back in ONE script on the device, and each leg is timed there (Stopwatch), as the product sees it.
DEVICE_LEGS = r'''
$pb=$req.pb
function Call($argv,$ms){$p=New-Object Diagnostics.Process;$p.StartInfo.FileName=$pb
$p.StartInfo.Arguments=(($argv|ForEach-Object{if($_ -match '\s'){'"'+$_+'"'}else{$_}}) -join ' ')
$p.StartInfo.RedirectStandardOutput=$true;$p.StartInfo.RedirectStandardError=$true;$p.StartInfo.UseShellExecute=$false;$p.StartInfo.CreateNoWindow=$true
[void]$p.Start();$o=$p.StandardOutput.ReadToEndAsync();$e=$p.StandardError.ReadToEndAsync()
if(-not $p.WaitForExit([int][math]::Max(1,$ms))){$p.Kill();return @{rc=-1;out='';err='no answer'}};@{rc=$p.ExitCode;out=$o.Result;err=$e.Result}}
$res=foreach($l in $req.legs){$r=Call @('service','restart') 120000
if($r.rc){throw ('pb service restart failed: '+$r.rc)}
$w=[Diagnostics.Stopwatch]::StartNew();$ok=$false;$why='no attempt'
$limit=[double]$l.deadline
while(-not $ok -and $w.Elapsed.TotalSeconds -lt $limit){$r=Call (@($l.command,$l.target,'--')+@($l.argv)) (($limit-$w.Elapsed.TotalSeconds)*1000)
$ok=($r.rc -eq 0) -and (($r.out -split "\r?\n") -contains [string]$l.expected);if(-not $ok){$why='exit '+$r.rc+': '+$r.err.Trim();Start-Sleep -Milliseconds 250}}
[pscustomobject]@{leg=$l.label;ok=$ok;seconds=[math]::Round($w.Elapsed.TotalSeconds,1);limit=$limit;why=$why}}
[Console]::Out.Write((ConvertTo-Json -InputObject @($res) -Compress))
'''


def legs_verdict(results):
    """One line per leg with its time inside Windows; a RuntimeError naming every leg that missed the deadline or failed."""
    for r in results:
        print('%s %s %.1fs inside Windows (limit %.0fs)' % ('PASS' if r['ok'] else 'FAIL', r['leg'], r['seconds'], r['limit']))
    failed = ['%s: %s after %.1fs, over its %.0fs limit' % (r['leg'], r['why'], r['seconds'], r['limit']) for r in results if not r['ok']]
    if failed:
        raise RuntimeError('still failing after the restart: ' + '; '.join(failed))


def leg_order(own, peer):
    """(command, label, target) of the four legs, each cold."""
    return [(command, label, target) for command in ('exec', 'ssh') for label, target in (('self', own), ('peer', peer))]


def windows_legs(binary, own, peer):
    legs = []
    for command, label, target in leg_order(own, peer):
        argv, expected = probe(binary, target, command)
        legs.append({'label': '%s %s' % (command, label), 'command': command, 'target': target, 'argv': argv, 'expected': expected, 'deadline': DEADLINE[command]})
    out = binary.device.ps('exec-ssh-after-restart', DEVICE_LEGS, timeout=300, pb=binary.exe, legs=legs)[1]
    legs_verdict(json.loads(out))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('peer')
    parser.add_argument('--self', dest='own', help='this machine\'s alias (skips the machine list lookup: 128 machines take minutes on Windows)')
    args = parser.parse_args()
    binary = winhost.pb_path()
    own = args.own or next(m['alias'] for m in invoke(binary, 'machine', 'list', '--all', '--json')['machines']
                           if m['id'] == registration()['machine_id'])
    if hasattr(binary, 'device'):  # a Windows client: every restart and leg run inside the device
        windows_legs(binary, own, args.peer)
        print('{"check": "exec-ssh-after-restart", "status": "PASS", "deadline_s": {"exec": %d, "ssh": %d}}' % (DEADLINE['exec'], DEADLINE['ssh']))
        return
    before = registration()
    steps = []
    for command, label, target in leg_order(own, args.peer):
        argv, expected = probe(binary, target, command)
        steps.append(('%s %s' % (command, label), lambda c=command, t=target, a=argv, e=expected: cold(binary, c, t, a, e)))
    # One after the other, each from its own restart with its own 5 s: a blackout after any restart fails its leg; four at once
    # contend for the same connections and are slower than any one.
    run_all(*steps)
    if registration() != before:
        raise RuntimeError('the daemon restarts changed the enrolled identity')
    print(json.dumps({'check': 'exec-ssh-after-restart', 'target': args.peer, 'status': 'PASS', 'enrollment_unchanged': True,
                      'deadline_s': DEADLINE}))


if __name__ == '__main__':
    main()
