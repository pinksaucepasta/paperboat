#!/usr/bin/env python3
"""F16-01..03 / C243: ENV reaches plain ssh shells through `pb env hook`.

  env_ssh_hook.py SSH_HOST

Runs from a controller over plain ssh. The host's pb must be logged in with an unlocked ENV vault
for the active workspace. Sets a random personal probe value, installs the bash hook (twice: one
line), then checks that a non-interactive `ssh host cmd` and an interactive ssh shell see the
value (compared by SHA-256, never printed), that a cached hook run is fast, that a missing runtime
or missing login loads nothing within the budget, and that uninstall removes only its line.
Cleans up the probe and the hook on every exit.
"""
import hashlib
import json
import secrets
import subprocess
import sys
import time

PB = '$HOME/.local/bin/pb'
PROBE = 'PB_ENV_HOOK_PROBE'
MARKER = '# paperboat-env-hook'


def ssh(host, command, stdin=None, tty=False, timeout=120):
    args = ['ssh', '-o', 'BatchMode=yes'] + (['-tt'] if tty else []) + [host]
    if command is not None:
        args.append(command)
    return subprocess.run(args, input=stdin, capture_output=True, text=True, timeout=timeout)


def must(result, step):
    if result.returncode:
        raise RuntimeError(f'{step} failed (exit {result.returncode})')
    return result.stdout


def probe_hash_line(output):
    for line in output.splitlines():
        if line.startswith('PROBE '):
            return line.split()[1]
    return None


def main():
    host = sys.argv[1]
    value = secrets.token_urlsafe(24)
    want = hashlib.sha256(value.encode()).hexdigest()
    report = {'host': host}
    rc_before = must(ssh(host, 'cat ~/.bashrc 2>/dev/null; true'), 'read bashrc')
    try:
        must(ssh(host, f'{PB} env set {PROBE} --value-stdin', stdin=value), 'env set')
        must(ssh(host, f'{PB} env hook bash --install && {PB} env hook bash --install'), 'hook install')
        count = int(must(ssh(host, f'grep -c -- "{MARKER}" ~/.bashrc'), 'count').strip())
        report['hook_lines'] = count
        assert count == 1, 'install is not idempotent'
        show = f'printf "PROBE %s\\n" "$(printf %s "${PROBE}" | sha256sum | cut -d" " -f1)"'
        report['noninteractive'] = probe_hash_line(must(ssh(host, show), 'ssh host cmd')) == want
        interactive = ssh(host, None, stdin=show + '\nexit\n', tty=True)
        report['interactive'] = probe_hash_line(interactive.stdout.replace('\r', '')) == want
        emit = f'{PB} env hook --emit bash >/dev/null'
        timing = must(ssh(host, f'{emit}; s=$(date +%s%N); {emit}; e=$(date +%s%N); echo $(((e-s)/1000000))'),
                      'timing')
        report['cached_ms'] = int(timing.strip().splitlines()[-1])
        quiet = (f's=$(date +%s%N); a=$(PAPERBOAT_RUNTIME_STATE_ROOT=/nonexistent {PB} env hook --emit bash 2>&1);'
                 f' b=$(PAPERBOAT_CONFIG=/nonexistent/config.json {PB} env hook --emit bash 2>&1);'
                 f' e=$(date +%s%N); echo "${{#a}} ${{#b}} $(((e-s)/1000000))"')
        no_runtime, no_login, quiet_ms = map(int, must(ssh(host, quiet), 'quiet paths').split())
        report['quiet_output_bytes'] = no_runtime + no_login
        report['quiet_ms'] = quiet_ms
        must(ssh(host, f'{PB} env hook --uninstall'), 'uninstall')
        rc_after = must(ssh(host, 'cat ~/.bashrc 2>/dev/null; true'), 'read bashrc')
        report['uninstall_restores_rc'] = rc_after == rc_before
        report['ok'] = (report['noninteractive'] and report['interactive'] and report['cached_ms'] < 500
                        and report['quiet_output_bytes'] == 0 and quiet_ms < 3000
                        and report['uninstall_restores_rc'])
    finally:
        ssh(host, f'{PB} env hook --uninstall >/dev/null 2>&1; {PB} env unset {PROBE} >/dev/null 2>&1')
    print(json.dumps(report, sort_keys=True))
    return 0 if report.get('ok') else 1


if __name__ == '__main__':
    sys.exit(main())
