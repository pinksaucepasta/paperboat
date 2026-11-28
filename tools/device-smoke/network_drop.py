#!/usr/bin/env python3
"""F3-03: a held terminal survives a network drop: same shell, no lost or duplicated input.

Cuts the client's network for a bounded time, typing before, during and after. On a real Linux machine (systemd, no
container) host_cut.sh cuts Paperboat's units and the check's own cgroup (IPv4 and IPv6) with a systemd failsafe timer.
On macOS a pf anchor under com.apple blocks the CLI user (both families) except the established TCP connections that
are not pb's (the controller's SSH or tunnel, a hosted runner's job agent). In a container it is an iptables owner-drop
(IPv4). Each is removed on every exit path and by a root failsafe timer.
Usage: network_drop.py TARGET [--drop-seconds N]   (needs passwordless or cached sudo)

With PB_SMOKE_WINDOWS_PB set the client is the Windows device (winhost.py; TARGET is a Unix machine): the drop is made
on the peer (SMOKE_PEER_CUT, container_cut.sh HOST CONTAINER), because a Windows Firewall rule only blocks new flows and every pb.exe
must keep running for the check to test resume.
"""
import argparse
import atexit
import ipaddress
import json
import os
from pathlib import Path
import datetime
import glob
import re
import subprocess
import sys
import time
import uuid

from session_cleanup import cleanup
from session_lifecycle import keep_state, ready, show_state
from terminal_lifecycle import Terminal
import windows_target
import winhost

PB = winhost.pb_path()
DEVICE = winhost.device()
RULE = ['OUTPUT', '-m', 'owner', '--uid-owner', str(os.getuid()), '!', '-o', 'lo', '-j', 'DROP']
DARWIN = not DEVICE and os.uname().sysname == 'Darwin'
DIAGNOSTICS = Path.home() / ('Library/Application Support/Paperboat/state/diagnostics' if DARWIN
                             else '.local/state/paperboat/diagnostics')
# The stock macOS pf.conf evaluates com.apple/* anchors, so no change to the main ruleset is needed.
ANCHOR = 'com.apple/pbsmoke'
PF_RULE = 'block drop out quick on ! lo0 proto { tcp udp } all user %d\n' % os.getuid()
# A real Linux machine: the controller's SSH tunnel and a hosted runner's job run as the same user as pb, and some pb
# services run as root, so an owner-drop would sever the first and miss the second. A container has neither.
UNIT_CUT = (not DEVICE and not DARWIN and os.path.isdir('/run/systemd/system')
            and subprocess.run(['systemd-detect-virt', '--quiet', '--container']).returncode != 0)
CGROUPS = Path('/sys/fs/cgroup')
CLIENT_CGROUP = 'pbsmoke-client-%d' % os.getpid()
GUARD_CGROUP = 'system.slice/paperboat-machineguard.service'


def sudo(*args, input=None):
    return subprocess.run(['sudo', '-n', *args], capture_output=True, text=True, timeout=30, input=input)


def pf_keep_rules():
    """pf can match a user but not a program. Every established TCP connection of this user that is not pb's keeps passing,
    to that address and port only: on a hosted Mac the controller's SSH tunnel and the runner's job agent are among them,
    on a physical one the controller's sshd. The rules keep no state, so they also pass after the states are flushed."""
    listing = sudo('lsof', '-nP', '-a', '-u', str(os.getuid()), '-iTCP', '-sTCP:ESTABLISHED', '-Fcn').stdout
    owners, command = {}, ''
    for line in listing.splitlines():
        if line.startswith('c'):
            command = line[1:]
        elif line.startswith('n') and '->' in line:
            host, port = line.split('->', 1)[1].rsplit(':', 1)
            address = ipaddress.ip_address(host.strip('[]').split('%')[0])
            address = getattr(address, 'ipv4_mapped', None) or address
            if not address.is_loopback:
                owners.setdefault((str(address), int(port)), set()).add(command)
    shared = sorted('%s:%d' % key for key, commands in owners.items() if 'pb' in commands and len(commands) > 1)
    if shared:
        raise RuntimeError('pb and another program both talk to %s: the cut cannot tell them apart, not cut' % ', '.join(shared))
    return ''.join('pass out quick on ! lo0 proto tcp from any to %s port %d user %d flags any no state\n'
                   % (address, port, os.getuid()) for (address, port), commands in sorted(owners.items()) if 'pb' not in commands)


def pf_dropped():
    """Packets the anchor's block rules have dropped (`pfctl -v` prints counter lines under each rule)."""
    dropped, blocking = 0, False
    for line in sudo('pfctl', '-a', ANCHOR, '-v', '-s', 'rules').stdout.splitlines():
        if not line.lstrip().startswith('['):
            blocking = line.startswith('block')
        elif blocking:
            found = re.search(r'Packets: (\d+)', line)
            dropped += int(found.group(1)) if found else 0
    return dropped


def pf_block(rules, failsafe_seconds):
    """macOS: load rules into the anchor, enable pf, cut existing states. Returns (enable token, root failsafe)."""
    failsafe = subprocess.Popen(['sudo', '-n', 'sh', '-c', 'sleep %d; /sbin/pfctl -a %s -F all' % (failsafe_seconds, ANCHOR)],
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
    if sudo('pfctl', '-a', ANCHOR, '-f', '-', input=rules).returncode:
        failsafe.terminate()
        raise RuntimeError('pfctl could not load the drop rule into ' + ANCHOR)
    token = re.search(r'Token : (\d+)', sudo('pfctl', '-E').stderr)
    sudo('pfctl', '-F', 'states')  # an established connection keeps passing on its state until flushed
    return token.group(1) if token else None, failsafe


def pf_release(block):
    token, failsafe = block
    sudo('pfctl', '-a', ANCHOR, '-F', 'all')
    if token:
        sudo('pfctl', '-X', token)
    if failsafe.poll() is None:
        failsafe.terminate()


def shell_pid(raw):
    """The shell's pid in the text after PB_PID_: a ConPTY client repaints escape sequences around it."""
    found = re.search(rb'\d+', raw)
    if not found:
        raise RuntimeError('no shell pid in %r' % raw)
    return found.group()


def diagnostic_lines():
    """Every line of the daemon's diagnostic event files (on the device for a Windows client)."""
    if DEVICE:
        return DEVICE.matching_lines(DEVICE.runtime_dir().parent / 'state' / 'diagnostics', '*.ndjson', '"severity":"(error|warning)"')
    return (line for path in glob.glob(str(DIAGNOSTICS / '*.ndjson')) for line in open(path, errors='replace'))


def failures_since(moment):
    count = 0
    for line in diagnostic_lines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if event.get('at', '') >= moment and event.get('severity') in ('error', 'warning') \
                and event.get('category') in ('control_request', 'peer_connect', 'stream_open', 'transport'):
            count += 1
    return count


def join_cgroup():
    """Called before a check starts its first pb: on a real Linux machine the cut takes this process's own cgroup with
    Paperboat's units, and a process or socket that already exists would stay outside it."""
    if not UNIT_CUT:
        return
    home = Path('/proc/self/cgroup').read_text().strip().rsplit('::', 1)[1]
    mine = CGROUPS / CLIENT_CGROUP
    done = sudo('sh', '-c', 'mkdir -p %s && echo %d > %s/cgroup.procs' % (mine, os.getpid(), mine))
    if done.returncode:
        raise RuntimeError('could not move this check into a cgroup of its own: ' + done.stderr.strip())
    atexit.register(leave_cgroup, mine, '%s%s' % (CGROUPS, home))


def leave_cgroup(mine, home):
    """Moves everything in the check's cgroup back and removes it: the shell doing so is in it too (it inherited it)."""
    sudo('sh', '-c', 'for p in $(cat %s/cgroup.procs); do echo $p >%s/cgroup.procs; done 2>/dev/null; rmdir %s' % (mine, home, mine))


def unit_cut(*action, failsafe=900):
    """host_cut.sh on this machine, with this check's cgroup and the machine guard cut too."""
    also = [group for group in (CLIENT_CGROUP, GUARD_CGROUP) if (CGROUPS / group).is_dir()]
    if action[0] == 'add' and CLIENT_CGROUP not in also:
        raise RuntimeError('join_cgroup() did not run: the pb this check starts would not be cut')
    try:
        done = subprocess.run([str(Path(__file__).resolve().parent / 'host_cut.sh'), 'local', str(os.getuid()), *action],
                              capture_output=True, text=True, timeout=120,
                              env=dict(os.environ, PB_CUT_FAILSAFE=str(failsafe), PB_CUT_ALSO=' '.join(also)))
    except subprocess.TimeoutExpired:
        raise RuntimeError('client cut %s did not finish in 120 s' % action[0])
    if done.returncode:
        raise RuntimeError('client cut %s failed: %s' % (action[0], done.stderr.strip()[-300:]))
    return done.stdout.strip()


def require_cut_tool():
    command = ['nft', 'list', 'tables'] if UNIT_CUT else ['pfctl', '-s', 'info'] if DARWIN else ['iptables', '-S', 'OUTPUT']
    if sudo(*command).returncode:
        raise RuntimeError('needs sudo without a prompt for ' + command[0])


def cut(seconds):
    """Drop this client's outbound traffic for at most `seconds` (a root failsafe or a timer on the device lifts it
    whatever happens to this process); returns (block, failsafe) for release() and failsafe.terminate()."""
    if winhost.PEER_CUT:
        return winhost.peer_cut('add', 'all'), None
    if DEVICE:
        raise RuntimeError('a Windows client needs SMOKE_PEER_CUT (container_cut.sh HOST CONTAINER)')
    if DARWIN:
        return pf_block(pf_keep_rules() + PF_RULE, seconds), None
    if UNIT_CUT:
        unit_cut('add', 'all', failsafe=seconds)
        return None, None
    failsafe = subprocess.Popen(['sudo', '-n', 'sh', '-c', 'sleep %d; iptables -D %s' % (seconds, ' '.join(RULE))],
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
    sudo('iptables', '-I', *RULE)
    return None, failsafe


def release(block):
    """Lift the cut. Returns its proof (None for a container's iptables rule); raises, with the cut already lifted, when
    it did not hold."""
    if winhost.PEER_CUT:
        return winhost.peer_cut('remove')
    if DARWIN:
        dropped = pf_dropped()
        pf_release(block)
        if not dropped:
            raise RuntimeError('not cut: the pf block rules dropped nothing (or the failsafe had removed them)')
        return 'pf cut of uid %d: dropped=%d' % (os.getuid(), dropped)
    if UNIT_CUT:
        return winhost.proven_cut(unit_cut('remove'))
    sudo('iptables', '-D', *RULE)
    return None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--drop-seconds', type=int, default=20)
    args = parser.parse_args()
    if not DEVICE:
        require_cut_tool()
    join_cgroup()
    name = 'smoke-drop-' + uuid.uuid4().hex[:10]
    log = '~/.' + name + '.log'  # expanded by the target's shell: the terminal and pb exec share its home, not /tmp
    windows = windows_target.is_windows(PB, args.target)  # asked while the network is up
    windows_log = "(Join-Path $env:USERPROFILE '.%s.log')" % name
    terminal = None
    blocked = False
    failsafe = block = None
    try:
        terminal = Terminal(winhost.terminal_argv(PB, ['connect', args.target, '--name', name, '--status-bar', 'off']), env=winhost.terminal_env(PB))
        ready(terminal, windows)
        if windows:  # cmd.exe cannot name its pid: a variable only this shell holds proves it is the same one
            nonce = uuid.uuid4().hex
            keep_state(terminal, windows, nonce)
        else:
            terminal.send("pb_hold=$$; printf '\\nPB_PID_%s\\n' \"$$\"\r")
            terminal.expect('PB_PID_')
            held = shell_pid(terminal.expect('\n'))

        def type_line(number):
            # the shell computes the token: the typed text never contains it, so an echo of our input cannot match
            if windows:  # the caret keeps the typed line from matching; the redirect comes first so no digit precedes >>
                terminal.send('>>"%%USERPROFILE%%\\.%s.log" echo IN_%d& echo PB_^ACK_%d\r' % (name, number, number + 1000))
                return
            terminal.send("echo IN_%d >> %s; printf '\\nPB_%s_%s\\n' ACK $((%d+1000))\r" % (number, log, '%s', '%s', number))

        for number in (1, 2, 3):
            type_line(number)
            terminal.expect('PB_ACK_%d' % (number + 1000))
        started = datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%S')
        block, failsafe = cut(args.drop_seconds + 60)
        blocked = True
        time.sleep(4)
        for number in (4, 5, 6):  # typed while the network is down
            type_line(number)
            time.sleep(1)
        time.sleep(max(0, args.drop_seconds - 7))
        release(block)
        blocked = False
        for number in (4, 5, 6):
            terminal.expect('PB_ACK_%d' % (number + 1000), timeout=150)
        type_line(7)
        terminal.expect('PB_ACK_1007', timeout=60)
        if windows:
            show_state(terminal, windows, nonce)  # another shell would not know the variable
        else:
            terminal.send("printf '\\nPB_AGAIN_%s\\n' \"$$\"\r")
            terminal.expect('PB_AGAIN_', timeout=30)
            again = shell_pid(terminal.expect('\n'))
            if again != held:
                raise RuntimeError('terminal came back in a different shell (%r, was %r)' % (again, held))
        if not failures_since(started):
            raise RuntimeError('the network block never produced a connection failure; the drop was not exercised')
        # the shell that wrote the log runs on the target: read it there
        read = windows_target.ps_argv('Get-Content -LiteralPath ' + windows_log) if windows else ['sh', '-c', 'cat ' + log]
        lines = winhost.run_pb(PB, ['exec', args.target, '--', *read], 60).stdout.decode().split()
        if lines != ['IN_%d' % n for n in range(1, 8)]:
            raise RuntimeError('input lost, duplicated or reordered: %d lines' % len(lines))
    finally:
        if blocked:
            try:
                release(block)
            except (RuntimeError, subprocess.SubprocessError) as error:  # the first failure is the one to report
                print(error, file=sys.stderr, flush=True)
        if failsafe is not None and failsafe.poll() is None:
            failsafe.terminate()
        if terminal is not None:
            terminal.close()
        remove = (windows_target.ps_argv('Remove-Item -LiteralPath %s -Force -ErrorAction SilentlyContinue' % windows_log)
                  if windows else ['sh', '-c', 'rm -f ' + log])
        winhost.run_pb(PB, ['exec', args.target, '--', *remove], 60)
        try:
            cleanup(PB, args.target, name)
        except Exception:
            pass
    print(json.dumps({'check': 'network-drop', 'target': args.target, 'status': 'PASS',
                      'drop_seconds': args.drop_seconds, 'same_shell': True, 'input_exactly_once': True}), flush=True)


if __name__ == '__main__':
    main()
