#!/usr/bin/env python3
"""multi_account_host_windows.py HOST BINARY WINDOWS PB_EXE MINT_B  (F23-01 on Windows)

multi_account_host.sh with the two-user host being a Windows machine instead of a container: its host side runs
inside one container shell on HOST, a Windows machine is driven from here over SSH, so this is its own file.
Two OS users on WINDOWS (the SSH host; user A is its login, user B a throwaway local administrator the script creates and removes), each paired to its own account, use machines of the same name at the same time. HOST is the Docker
host of fresh_pair_lib.sh (its pb is account A), BINARY the Linux build on this machine, PB_EXE the Windows build
on this machine, MINT_B a command on HOST that runs `pb` signed in to a second account.
Fresh throwaway containers: target TA in account A and TB in account B, both named N, each serving its own marker
on 127.0.0.1:PORT. Passes when, through the one host gateway (https://PORT.N.<domain>), user A reaches TA's marker
and user B reaches TB's, user B asking for user A's name gets no A marker, and both daemons are ready.
Prints one JSON line {"check":"multi-account-host-windows",...}; every machine is revoked by exact id (A with
cleanup_test_account.py, B with MINT_B), the containers are removed and both users' pb uninstalled on every exit path.
"""
import json
import os
import re
import secrets
import subprocess
import sys
import time

import winhost

HERE = os.path.dirname(os.path.abspath(__file__))
PORT = 18480
USER_B = 'pbmahu2'


def host(target, script, timeout=900):
    lib = open(os.path.join(HERE, 'fresh_pair_lib.sh')).read()
    done = subprocess.run(['ssh', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=15', target, 'bash -s'], input=lib + '\n' + script,
                          capture_output=True, text=True, timeout=timeout)
    return done.stdout + done.stderr


class User:
    """One OS user of the Windows machine with its own pb."""

    def __init__(self, target, password=None):
        self.device = winhost.WindowsDevice(target=target, password=password)
        self.pb = None

    def json(self, *args, timeout=420):
        out = self.device.run([self.pb, *args, '--json'], timeout=timeout).stdout
        return json.loads(out[out.index('{'):])

    def install_and_pair(self, build, token, name):
        self.pb = build
        receipt = self.json('install', timeout=600)
        assert receipt.get('ok'), 'install failed: ' + json.dumps(receipt.get('error', {}))[:200]
        self.pb = receipt['data']['executable']
        token_file = self.device.home() / 'pb-mah.token'
        token_file.write_text(token)
        try:
            receipt = self.json('pair', '--enrollment-token-file', str(token_file), '--name', name)
        finally:
            token_file.unlink()
        assert receipt.get('ok'), 'pair failed: ' + json.dumps(receipt.get('error', {}))[:200]
        return receipt['data']['machine_id']

    def domain(self):
        data = self.json('config', 'local-access', 'show')['data']
        return data.get('domain') or data['local_access']['domain']

    def fetch(self, hostname):
        """Body of the marker through the gateway, empty on failure."""
        try:
            return self.device.run(['curl.exe', '-sk', '--noproxy', '*', '--connect-timeout', '10', '--max-time', '20',
                                    'https://%s/marker' % hostname], timeout=40).stdout.strip()
        except (winhost.DeviceError, subprocess.TimeoutExpired):
            return ''

    def health(self):
        data = self.json('status')
        data = data.get('data', data)
        return ' '.join([str(data.get('daemon_state'))] + [str(item.get('code')) for item in data.get('health', [])])

    def uninstall(self):
        if self.pb:
            try:
                name = self.device.run(['hostname.exe']).stdout.strip()
                self.device.run([self.pb, 'uninstall', '--confirmation', 'UNINSTALL PAPERBOAT', '--hostname', name], timeout=600)
            except (winhost.DeviceError, subprocess.TimeoutExpired):
                pass


def main():
    target, binary, windows, exe, mint_b = sys.argv[1:6]
    stamp = int(time.time())
    ta, tb, name = 'mah-ta-%d' % stamp, 'mah-tb-%d' % stamp, 'winpool-mah-%d' % stamp
    # User B is a throwaway local administrator (an SSH session is elevated, as pb install needs) with a random
    # password; it and its profile are removed on every exit path.
    password = secrets.token_urlsafe(18) + 'aA1!'
    user_a, user_b = User(windows), User('%s@%s' % (USER_B, windows), password if os.environ.get('VICTUS_PASS') else '')
    shared = r'C:\Users\Public\pb-mah.exe'
    ids_a, ids_b, detail, status = [], [], '', 'FAIL'
    try:
        subprocess.run(['scp', '-q', binary, '%s:/tmp/%s-pb' % (target, ta)], check=True, timeout=600)
        out = host(target, f"""
set -euo pipefail
FRESH_BINARY=/tmp/{ta}-pb
origin() {{ sudo -n docker exec -u pbt -e HOME=/home/pbt $1 bash -c "mkdir -p ~/origin && echo $2 > ~/origin/marker && cd ~/origin && nohup python3 -m http.server {PORT} --bind 127.0.0.1 >/dev/null 2>&1 &"; }}
fresh {ta} {name} | sed 's/^MACHINE_ID=/A_MACHINE_ID=/'
FRESH_MINT="{mint_b}" fresh {tb} {name} | sed 's/^MACHINE_ID=/B_MACHINE_ID=/'
origin {ta} MARKER_A_{stamp}
origin {tb} MARKER_B_{stamp}
echo "A_ADD=$($HOME/.local/bin/pb machine add --name {name}-ha --json | tr -d '\\n')"
echo "B_ADD=$({mint_b} machine add --name {name}-hb --json | tr -d '\\n')"
""")
        ids_a += re.findall(r'^A_MACHINE_ID=(machine_[0-9a-f-]+)$', out, re.M)
        ids_b += re.findall(r'^B_MACHINE_ID=(machine_[0-9a-f-]+)$', out, re.M)
        tokens = {}
        for side in 'AB':
            added = re.search(r'^%s_ADD=(\{.*\})$' % side, out, re.M)
            assert added, 'targets were not prepared on %s: %s' % (target, re.sub(r'_ADD=.*', '_ADD=…', out)[-400:])
            tokens[side] = re.search(r"p=([^'\"&]+)", json.loads(added.group(1))['data']['windows_command']).group(1)[-26:]
        scp = ['scp', '-q', '-o', 'StrictHostKeyChecking=accept-new', exe, '%s:/%s' % (windows, shared.replace('\\', '/'))]
        subprocess.run((['sshpass', '-e'] + scp) if user_a.device.password else (scp[:1] + ['-o', 'BatchMode=yes'] + scp[1:]), check=True, timeout=600, env=user_a.device.ssh_environment())
        user_a.device.run(['net.exe', 'user', USER_B, password, '/add', '/y'])
        user_a.device.run(['net.exe', 'localgroup', 'Administrators', USER_B, '/add'])
        ids_a.append(user_a.install_and_pair(shared, tokens['A'], name + '-ha'))
        ids_b.append(user_b.install_and_pair(shared, tokens['B'], name + '-hb'))
        guard = user_a.device.run([user_a.pb, 'daemon', 'machine-guard', 'install'], timeout=300)
        assert guard.returncode == 0, 'machine guard install failed: ' + (guard.stdout + guard.stderr)[-300:]
        domain_a, domain_b = user_a.domain(), user_b.domain()
        a = b = ''
        for _ in range(60):
            a, b = user_a.fetch('%d.%s.%s' % (PORT, name, domain_a)), user_b.fetch('%d.%s.%s' % (PORT, name, domain_b))
            if a == 'MARKER_A_%d' % stamp and b == 'MARKER_B_%d' % stamp:
                break
            time.sleep(3)
        cross = user_b.fetch('%d.%s.%s' % (PORT, name, domain_a))
        health_a, health_b = user_a.health(), user_b.health()
        detail = 'A_GOT=%s B_GOT=%s CROSS=%s A_HEALTH=%s B_HEALTH=%s' % (a, b, cross[:40], health_a, health_b)
        assert a == 'MARKER_A_%d' % stamp, "user A did not reach its own account's machine"
        assert b == 'MARKER_B_%d' % stamp, "user B did not reach its own account's machine"
        assert cross != 'MARKER_A_%d' % stamp, "user B reached user A's machine through the gateway"
        assert health_a == 'ready' and health_b == 'ready', 'a daemon is not ready or a local access item is degraded'
        status = 'PASS'
        detail = ('two OS users on one Windows host, two accounts, same machine name: each reached its own account\'s machine '
                  'through the one gateway, the second user could not reach the first\'s, and both daemons are ready; ' + detail)
    except Exception as error:  # every failure is the check's verdict
        detail = ('%s; %s' % (str(error)[:300], detail)).strip('; ')
    finally:
        for user in (user_b, user_a):
            user.uninstall()
        try:  # the throwaway user and its profile, and the shared copy of the build
            user_a.device.run(['net.exe', 'user', USER_B, '/delete'])
            user_a.device.run(['cmd.exe', '/c', 'rmdir /s /q C:\\Users\\%s & del /f /q %s' % (USER_B, shared)])
        except (winhost.DeviceError, subprocess.TimeoutExpired):
            detail += '; the throwaway user %s was not removed' % USER_B
        cleaned = host(target, 'sudo -n docker rm -f %s %s >/dev/null 2>&1; rm -f /tmp/%s-pb\n' % (ta, tb, ta) + ''.join(
            # B's machines are revoked by exact id with the second account's own pb.
            '%s machine revoke %s --json --confirm "$(%s machine revoke %s --json | python3 -c \'import json,sys;print(json.load(sys.stdin)["data"]["confirmation_token"])\')" >/dev/null 2>&1 || echo REVOKE_FAILED %s\n'
            % (mint_b, mid, mint_b, mid, mid) for mid in ids_b), timeout=300)
        detail += ''.join('; revoke of %s failed' % mid for mid in re.findall(r'REVOKE_FAILED (\S+)', cleaned))
        for mid in ids_a:
            done = subprocess.run([sys.executable, os.path.join(HERE, 'cleanup_test_account.py'), '--id', mid, '--stale', '0 seconds', '--apply'],
                                  capture_output=True, text=True)
            if done.returncode:
                detail += '; revoke of %s failed' % mid
    print(json.dumps({'check': 'multi-account-host-windows', 'status': status, 'machine_ids': ids_a,
                      'second_account_machine_ids': ids_b, 'detail': detail}))
    return 0 if status == 'PASS' else 1


if __name__ == '__main__':
    sys.exit(main())
