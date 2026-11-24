#!/usr/bin/env python3
"""pb ssh works after the target's ssh-target-port was saved as an unusable port, a new runtime worker started, and
the port went back to 22.

Run from the client against TARGET: the port is the loopback port the target's own sshd answers on, set ON the target
(`pb config set ssh-target-port N`, here through pb exec). With 2201 saved, a same-version `pb update --from` starts a
new runtime worker; putting 22 back must leave `pb ssh TARGET` working within SETTLE seconds. Before the fix that
worker kept dialling the unusable port, so every ssh ended with zero bytes for minutes while doctor stayed green (a
change without a worker restart recovered, so that alone proves nothing). The answer is a sum the target's shell
computes. The port is put back to 22 on every exit path."""
import argparse
import subprocess
import sys
import time

from exec_ssh_after_restart import attempt, probe
import windows_target
import winhost

UNUSED_PORT = 2201
SETTLE = 30.0
RECONNECT = 60.0  # the update restarts the daemon that carries the pb exec calls


def set_port(binary, target, port):
    """Run `pb config set ssh-target-port PORT` on the target."""
    if windows_target.is_windows(binary, target):
        argv = [windows_target.target_pb(binary, target), 'config', 'set', 'ssh-target-port', str(port)]
    else:
        argv = ['sh', '-c', '~/.local/bin/pb config set ssh-target-port %d' % port]
    result = winhost.run_pb(binary, ['exec', target, '--', *argv], 60)
    if result.returncode:
        raise RuntimeError('pb config set ssh-target-port %d on %s failed: %s' % (port, target, result.stderr.decode(errors='replace')[-200:]))


def restart_worker(binary, target):
    """A same-version `pb update --from` on the target: starts a new runtime worker. The exec that carries it may
    be cut when the daemon restarts, so its exit is not checked; set_port_retry then proves the target answers."""
    if windows_target.is_windows(binary, target):
        pb = windows_target.target_pb(binary, target)
        quoted = pb.replace("'", "''")
        argv = windows_target.ps_argv(
            "$p=Join-Path $env:TEMP 'pb-same.exe';Copy-Item -LiteralPath '%s' -Destination $p -Force;"
            "try{& '%s' update --from $p}finally{Remove-Item -LiteralPath $p -Force -ErrorAction SilentlyContinue}" % (quoted, quoted))
    else:
        argv = ['sh', '-c', 'cp ~/.local/bin/pb /tmp/pb-same && ~/.local/bin/pb update --from /tmp/pb-same; rm -f /tmp/pb-same']
    winhost.run_pb(binary, ['exec', target, '--', *argv], 120)


def set_port_retry(binary, target, port):
    deadline = time.monotonic() + RECONNECT
    while True:
        try:
            return set_port(binary, target, port)
        except (RuntimeError, subprocess.TimeoutExpired):
            if time.monotonic() > deadline:
                raise
            time.sleep(3)


def works(binary, target, deadline):
    argv, expected = probe(binary, target, 'ssh')
    attempt(binary, 'ssh', target, argv, expected, time.monotonic(), deadline=deadline)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    binary = winhost.pb_path()
    works(binary, args.target, SETTLE)  # the control: ssh works before anything is changed
    failure = None
    try:
        set_port(binary, args.target, UNUSED_PORT)
        restart_worker(binary, args.target)
        set_port_retry(binary, args.target, 22)
        try:
            works(binary, args.target, SETTLE)
        except RuntimeError as error:
            raise RuntimeError('pb ssh did not work again within %.0fs of the port going back to 22 on a worker started while 2201 was saved: %s' % (SETTLE, error)) from None
    except BaseException as error:
        failure = error
        raise
    finally:
        try:
            set_port_retry(binary, args.target, 22)
        except (RuntimeError, subprocess.TimeoutExpired) as error:
            if failure is None:
                raise
            failure.add_note('putting the port back to 22 failed: ' + str(error))
    print('{"check": "ssh-port-change", "status": "PASS", "settle_s": %d}' % SETTLE)


if __name__ == '__main__':
    main()
