#!/usr/bin/env python3
"""F16-04: VS Code Remote-SSH's server sees ENV loaded by `pb env hook`.

  env_vscode_remote.py SSH_HOST

Headless stand-in for the Remote-SSH extension: like it, start the real VS Code server through a
non-interactive `ssh HOST bash` with the launch script on stdin, then read the running server's
/proc/<pid>/environ. Terminals, tasks and the extension host inherit that environment. A server
started before the hook was installed keeps its old environment, which is the documented reload
step: "Remote-SSH: Kill VS Code Server on Host", then reconnect. The probe value is compared by
SHA-256, never printed. Cleans up the probe, the hook, the server and its files on every exit.
"""
import hashlib
import json
import secrets
import subprocess
import sys

PB = '$HOME/.local/bin/pb'
PROBE = 'PB_ENV_HOOK_PROBE'
DIR = '$HOME/.lanej-vscode-server'
# `pb env unset` previews first and needs the preview's confirmation code.
UNSET = (f'c=$({PB} env unset {PROBE} --json 2>/dev/null | python3 -c "import json,sys;'
         f'print(json.load(sys.stdin).get(\'data\',{{}}).get(\'confirmation_token\',\'\'))");'
         f' [ -n "$c" ] && {PB} env unset {PROBE} --json --confirm "$c" >/dev/null 2>&1')
SERVER_URL = 'https://update.code.visualstudio.com/latest/server-linux-x64/stable'

# What Remote-SSH does on the host: fetch the server once, start it detached, report its pid.
LAUNCH = f'''
set -e
mkdir -p {DIR}
if [ ! -x {DIR}/bin/code-server ]; then
  curl -fsSL '{SERVER_URL}' | tar -xz -C {DIR} --strip-components 1
fi
nohup {DIR}/bin/code-server --start-server --host=127.0.0.1 --port=0 --without-connection-token \\
  --accept-server-license-terms --telemetry-level off >{DIR}/server.log 2>&1 &
echo PID $!
'''

# Hash of the probe in the server's own environment (or "absent").
READ = f'''pid=$1
for i in $(seq 50); do [ -r /proc/$pid/environ ] && grep -q "listening\\|Extension host agent" {DIR}/server.log && break; sleep 0.2; done
v=$(tr '\\0' '\\n' </proc/$pid/environ | sed -n 's/^{PROBE}=//p')
[ -n "$v" ] && printf %s "$v" | sha256sum | cut -d' ' -f1 || echo absent
'''


def ssh(host, command, stdin=None, timeout=300):
    args = ['ssh', '-o', 'BatchMode=yes', '-T', host] + ([command] if command else [])
    return subprocess.run(args, input=stdin, capture_output=True, text=True, timeout=timeout)


def must(result, step):
    if result.returncode:
        raise RuntimeError(f'{step} failed (exit {result.returncode})')
    return result.stdout


def start_server(host):
    out = must(ssh(host, 'bash', stdin=LAUNCH), 'start VS Code server over ssh bash')
    return next(line.split()[1] for line in out.splitlines() if line.startswith('PID '))


def server_probe(host, pid):
    return must(ssh(host, f'bash -s {pid}', stdin=READ), 'read server environment').strip()


def stop_server(host, pid):
    ssh(host, f'kill {pid} 2>/dev/null; pkill -f "{DIR}" 2>/dev/null; true')


def main():
    host = sys.argv[1]
    value = secrets.token_urlsafe(24)
    want = hashlib.sha256(value.encode()).hexdigest()
    report = {'host': host}
    try:
        must(ssh(host, f'{PB} env set {PROBE} --value-stdin', stdin=value), 'env set')
        stale = start_server(host)
        must(ssh(host, f'{PB} env hook bash --install'), 'hook install')
        report['server_started_before_hook_has_probe'] = server_probe(host, stale) == want
        stop_server(host, stale)  # the documented reload step
        fresh = start_server(host)
        report['server_after_reload_has_probe'] = server_probe(host, fresh) == want
        stop_server(host, fresh)
        report['ok'] = report['server_after_reload_has_probe'] and not report['server_started_before_hook_has_probe']
    finally:
        ssh(host, f'pkill -f "{DIR}"; rm -rf {DIR}; {PB} env hook --uninstall >/dev/null 2>&1;'
                  f' {UNSET}; true')
    print(json.dumps(report, sort_keys=True))
    return 0 if report.get('ok') else 1


if __name__ == '__main__':
    sys.exit(main())
