#!/usr/bin/env python3
"""Preflight: warn when a test host holds many login sessions, fail when logind has plainly stopped retiring them.

pbvm3's systemd-logind wedged on 2026-11-13 and kept one `closing` session per ssh connection: 1,450 by 2026-11-20 (uptime showed
1,233 users). Every smoke ssh adds one, so a wedged host is found the day it happens, not a week later.

  host_sessions.py HOST...   exit 1 when a host has more than FAIL sessions; warnings go to stderr
"""
import subprocess
import sys

WARN = 50
FAIL = 500


def sessions(host, run=subprocess.run):
    """Logind sessions on host, or None when it cannot be counted (no systemd, unreachable): not this check's failure."""
    try:
        done = run(['ssh', '-o', 'ConnectTimeout=15', '-o', 'BatchMode=yes', host, 'loginctl list-sessions --no-legend | wc -l'],
                   capture_output=True, text=True, timeout=60)
        return int(done.stdout.strip()) if done.returncode == 0 else None
    except (OSError, ValueError, subprocess.TimeoutExpired):
        return None


def verdict(host, count):
    """('ok'|'warn'|'fail', message) for a host with count sessions."""
    if count is None:
        return 'warn', '%s: login sessions could not be counted' % host
    restart = 'ssh %s sudo systemctl restart systemd-logind  (kills no user processes)' % host
    if count > FAIL:
        return 'fail', '%s holds %d login sessions: systemd-logind stopped retiring them. Restart it when no check runs there: %s' % (host, count, restart)
    if count > WARN:
        return 'warn', '%s holds %d login sessions (more than %d): logind may be wedged; if it keeps growing: %s' % (host, count, WARN, restart)
    return 'ok', '%s: %d login sessions' % (host, count)


def main(hosts, run=subprocess.run):
    failed = False
    for host in hosts:
        status, message = verdict(host, sessions(host, run))
        if status != 'ok':
            print(('preflight FAIL: ' if status == 'fail' else 'preflight warning: ') + message, file=sys.stderr)
        failed = failed or status == 'fail'
    return 1 if failed else 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
