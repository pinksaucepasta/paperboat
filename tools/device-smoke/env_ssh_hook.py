#!/usr/bin/env python3
"""F16-01..03 / C243: ENV reaches plain ssh shells through `pb env hook`.

  env_ssh_hook.py SSH_HOST

Runs from a controller over plain ssh. The host's pb must be logged in with an unlocked ENV vault
for the active workspace. Sets a random personal probe value, installs the bash hook (twice: one
line), then checks that a non-interactive `ssh host cmd` and an interactive ssh shell see the
value (compared by SHA-256, never printed), that a cached hook run is fast, that a missing runtime
or missing login loads nothing within the budget, and that uninstall removes only its line.
Cleans up the probe and the hook on every exit.

A macOS host is detected over ssh (`uname -s`): its login shell is zsh, so the hook goes in ~/.zshenv
(zsh reads it for `ssh host cmd` and interactive shells), hashes use shasum and timing uses perl, because
macOS date has no %N. Over ssh the host's login keychain must already be unlocked for the ENV vault.
"""
import hashlib
import json
import secrets
import subprocess
import sys
import time

PB = '$HOME/.local/bin/pb'
PROBE = 'PB_ENV_HOOK_PROBE'
# `pb env unset` previews first and needs the preview's confirmation code.
UNSET = (f'c=$({PB} env unset {PROBE} --json 2>/dev/null | python3 -c "import json,sys;'
         f'print(json.load(sys.stdin).get(\'data\',{{}}).get(\'confirmation_token\',\'\'))");'
         f' [ -n "$c" ] && {PB} env unset {PROBE} --json --confirm "$c" >/dev/null 2>&1')

# macOS has no python3 guarantee on a non-interactive ssh PATH: pull the token out with sed.
TOKEN_SED = r"""sed -n 's/.*"confirmation_token" *: *"\([^"]*\)".*/\1/p'"""
UNSET_MAC = (f'c=$({PB} env unset {PROBE} --json 2>/dev/null | {TOKEN_SED});'
             f' [ -n "$c" ] && {PB} env unset {PROBE} --json --confirm "$c" >/dev/null 2>&1')

MARKER = '# paperboat-env-hook'


def ssh(host, command, stdin=None, tty=False, timeout=120):
    args = ['ssh', '-o', 'BatchMode=yes'] + (['-tt'] if tty else []) + [host]
    if command is not None:
        args.append(command)
    return subprocess.run(args, input=stdin, capture_output=True, text=True, timeout=timeout)


def account(host, command, stdin=None):
    """Run an account command (env set/unset). A Mac's login keychain, which holds the CLI token, is
    locked in every new ssh session, so a controller that is the Mac itself runs them locally."""
    if host in ('localhost', '127.0.0.1'):
        return subprocess.run(['/bin/sh', '-c', command], input=stdin, capture_output=True, text=True, timeout=120)
    return ssh(host, command, stdin=stdin)


def must(result, step):
    if result.returncode:
        raise RuntimeError(f'{step} failed (exit {result.returncode})')
    return result.stdout


def host_is_mac(host):
    return must(ssh(host, 'uname -s'), 'detect the host OS').strip() == 'Darwin'


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
    mac = host_is_mac(host)
    shell, rc, sha = ('zsh', '~/.zshenv', 'shasum -a 256') if mac else ('bash', '~/.bashrc', 'sha256sum')
    now, ms = ("perl -MTime::HiRes=time -e 'printf(\"%d\", time()*1000)'", '') if mac else ('date +%s%N', '/1000000')
    rc_before = must(ssh(host, f'cat {rc} 2>/dev/null; true'), 'read shell rc file')
    try:
        must(account(host, f'{PB} env set {PROBE} --value-stdin', stdin=value), 'env set')
        must(ssh(host, f'{PB} env hook {shell} --install && {PB} env hook {shell} --install'), 'hook install')
        count = int(must(ssh(host, f'grep -c -- "{MARKER}" {rc}'), 'count').strip())
        report['hook_lines'] = count
        assert count == 1, 'install is not idempotent'
        show = f'printf "PROBE %s\\n" "$(printf %s "${PROBE}" | {sha} | cut -d" " -f1)"'
        report['noninteractive'] = probe_hash_line(must(ssh(host, show), 'ssh host cmd')) == want
        interactive = ssh(host, None, stdin=show + '\nexit\n', tty=True)
        report['interactive'] = probe_hash_line(interactive.stdout.replace('\r', '')) == want
        emit = f'{PB} env hook --emit {shell} >/dev/null'
        timing = must(ssh(host, f'{emit}; s=$({now}); {emit}; e=$({now}); echo $(((e-s){ms}))'),
                      'timing')
        report['cached_ms'] = int(timing.strip().splitlines()[-1])
        quiet = (f's=$({now}); a=$(PAPERBOAT_RUNTIME_STATE_ROOT=/nonexistent {PB} env hook --emit {shell} 2>&1);'
                 f' b=$(PAPERBOAT_CONFIG=/nonexistent/config.json {PB} env hook --emit {shell} 2>&1);'
                 f' e=$({now}); echo "${{#a}} ${{#b}} $(((e-s){ms}))"')
        no_runtime, no_login, quiet_ms = map(int, must(ssh(host, quiet), 'quiet paths').split())
        report['quiet_output_bytes'] = no_runtime + no_login
        report['quiet_ms'] = quiet_ms
        must(ssh(host, f'{PB} env hook --uninstall'), 'uninstall')
        rc_after = must(ssh(host, f'cat {rc} 2>/dev/null; true'), 'read shell rc file')
        report['uninstall_restores_rc'] = rc_after == rc_before
        report['ok'] = (report['noninteractive'] and report['interactive'] and report['cached_ms'] < 500
                        and report['quiet_output_bytes'] == 0 and quiet_ms < 3000
                        and report['uninstall_restores_rc'])
    finally:
        ssh(host, f'{PB} env hook --uninstall >/dev/null 2>&1')
        account(host, UNSET_MAC if mac else UNSET)
    print(json.dumps(report, sort_keys=True))
    return 0 if report.get('ok') else 1


if __name__ == '__main__':
    sys.exit(main())
