#!/usr/bin/env python3
"""X-36: updating one machine (TARGET) mid-session keeps the other machine's (CLIENT) sessions.

  xplat_midupdate.py TARGET CLIENT CANDIDATE_DIR      (L|M|W each; environment of xplat_fleet.py)

CLIENT holds a terminal (`pb connect`, a shell variable), a `pb ssh` into TARGET, a running exec and a 150 MB `pb send` to TARGET; TARGET
serves a public tunnel. Update 1 (`pb update --from CANDIDATE`, no --force) must be refused naming the open SSH connection while the ssh
session stays alive; after the ssh is closed update 2 must succeed and then the same shell answers with its variable, the exec returns,
the transfer ends with equal bytes and the tunnel answers on the same URL.
"""
import hashlib
import json
import re
import subprocess
import sys
import threading
import time
import uuid
from pathlib import Path

import pexpect

HERE = Path(__file__).parent
sys.path.insert(0, str(HERE))
import xplat_fleet as fleet  # noqa: E402
import xplat_term as term_mod  # noqa: E402
import xplat_tunnel as tun  # noqa: E402
from xplat_send import inbox_listing, inbox_path, py  # noqa: E402

BINARIES = {'linux': 'pb-linux-amd64', 'darwin': 'pb-darwin-arm64', 'windows': 'pb-windows-amd64.exe'}
BIG = 150 * 1024 * 1024


def log(text):
    print(time.strftime('%H:%M:%S'), text, flush=True)


def spawn(client, tail):
    argv = term_mod.client_argv(client, [client.pb_path, *tail])
    return pexpect.spawn(argv[0], argv[1:], encoding='utf-8', codec_errors='replace', timeout=30, dimensions=(30, 200))


def sleeper(target, seconds):
    return [py(target), '-c', 'import time;time.sleep(%d);print("SLEPT_OK")' % seconds]


def put_candidate(target, path):
    if target.os == 'windows':
        remote = target.home + '\\pb-cand.exe'
        subprocess.run(['scp', '-q', str(path), '%s:%s' % (target.device.target, remote.replace('\\', '/'))], check=True, timeout=600)
        return remote
    remote = target.home + '/pb-cand'
    target.write(remote, Path(path).read_bytes(), mode='755')
    return remote


def version(dev):
    return dev.pb('version').text.strip().splitlines()[1].split()[-1]


def main():
    tkey, ckey, cand_dir = sys.argv[1:4]
    devices = fleet.fleet((tkey, ckey))
    target, client = devices[tkey], devices[ckey]
    before = version(target)
    log('target %s %s %s, client %s %s %s' % (target.name, target.os, before, client.name, client.os, version(client)))
    cand = put_candidate(target, Path(cand_dir) / BINARIES[target.os])
    ok = {}
    # terminal with a variable
    term = term_mod.Terminal(client, target)
    term.ready()
    setter = {'unix': 'export X36V=keep', 'powershell': "$env:X36V='keep'", 'cmd': 'set X36V=keep'}[term.kind]
    reader = {'unix': 'echo V_$X36V', 'powershell': 'echo ("V_" + $env:X36V)', 'cmd': 'echo V_%X36V%'}[term.kind]
    term.say(setter)
    time.sleep(2)
    log('terminal up (%s), variable set' % term.kind)
    # running exec
    exe = spawn(client, ['exec', target.alias, '--', *sleeper(target, 300)])
    # transfer
    src = client.home + ('\\x36mid.bin' if client.os == 'windows' else '/x36mid.bin')
    client.run([py(client), '-c', 'import os;open(r"%s","wb").write(os.urandom(%d))' % (src, BIG)], timeout=300)
    digest = client.sha256(src)
    send = spawn(client, ['send', src, '--to', target.alias])
    # tunnel on the target
    port = 31000 + int(uuid.uuid4().hex[:4], 16) % 2000
    marker = 'MID_' + uuid.uuid4().hex[:8]
    threading.Thread(target=lambda: target.run([py(target), '-c', tun.ORIGIN.replace('Timer(280', 'Timer(1500'), '127.0.0.1', str(port), marker], timeout=1500), daemon=True).start()
    time.sleep(3)
    name = 'x36mid-' + uuid.uuid4().hex[:4]
    created = fleet.json_pb(target, 'tunnel', 'create', name, '--port', str(port), '--wait', timeout=240)
    url = created.get('tunnel', created).get('stable_endpoint') or json.dumps(created)[:200]
    def fetch():
        done = subprocess.run(['curl', '-s', '-m', '20', '-o', '-', '-w', '\n%{http_code}', url + '/'], capture_output=True, text=True)
        return done.stdout.rsplit('\n', 1)
    for _ in range(12):
        body = fetch()
        if body[0] == marker and body[-1] == '200':
            break
        time.sleep(4)
    log('tunnel %s before update: %s' % (url, body[-1]))
    ok['tunnel_before'] = body[0] == marker
    time.sleep(10)
    # pb ssh held open
    ssh = spawn(client, ['ssh', target.alias, '--'] + (['ping', '-n', '45', '127.0.0.1'] if target.os == 'windows' else ['sleep', '900']))
    time.sleep(5)
    # update 1: refused while ssh is open
    log('update 1 (no --force) while pb ssh, exec and transfer are open')
    first = target.pb('update', '--from', cand, '--json', timeout=300)
    text = first.text + first.errtext
    log('  rc %s %s' % (first.rc, text.strip()[:300]))
    ok['update1_refused'] = first.rc != 0 and re.search(r'busy|open SSH', text, re.I) is not None
    ok['version_unchanged'] = version(target) == before
    ok['ssh_alive'] = ssh.isalive()
    log('ssh alive after refusal: %s; version %s' % (ok['ssh_alive'], version(target)))
    ssh.close(force=True)
    if client.os == 'windows':  # the ssh hop dying does not end pb ssh inside docker exec or a powershell: stop the client's `pb ssh` itself
        client.sh("Get-CimInstance Win32_Process -Filter \"Name='pb.exe'\" | Where-Object { $_.CommandLine -match ' ssh ' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }")
    else:
        client.sh("ps -eo pid,args | awk '$2 ~ /\\/pb$/ && $3==\"ssh\" {print $1}' | xargs -r kill")
    time.sleep(5)
    log('update 2 (no --force) after the ssh closed')
    for attempt in range(8):  # the client's pb ssh was killed: the target's count of open SSH connections drops a moment later
        second = target.pb('update', '--from', cand, '--json', timeout=600)
        if second.rc == 0 or 'update_busy' not in second.text + second.errtext:
            break
        log('  still busy, waiting')
        time.sleep(15)
    log('  rc %s %s' % (second.rc, (second.text + second.errtext).strip()[:200]))
    time.sleep(10)
    after = version(target)
    ok['updated'] = second.rc == 0 and after != before
    log('version after update 2: %s (before %s)' % (after, before))
    # same shell
    command, want = term.echo(uuid.uuid4().hex[:6])
    term.say(command)
    try:
        term.child.expect(want, timeout=90)
        term.say(reader)
        term.child.expect('V_keep', timeout=30)
        ok['same_shell'] = True
    except (pexpect.TIMEOUT, pexpect.EOF):
        ok['same_shell'] = False
    log('terminal: same shell answers with its variable: %s' % ok['same_shell'])
    try:
        exe.expect('SLEPT_OK', timeout=420)
        ok['exec_returned'] = True
    except (pexpect.TIMEOUT, pexpect.EOF):
        ok['exec_returned'] = False
    log('exec returned: %s' % ok['exec_returned'])
    try:
        send.expect(pexpect.EOF, timeout=900)
        send.close()
        ok['send_rc0'] = send.exitstatus == 0
    except pexpect.TIMEOUT:
        ok['send_rc0'] = False
    received = None
    ok['transfer_equal'] = digest in inbox_listing(target)['files'].values()
    log('transfer rc0 %s, bytes equal %s' % (ok['send_rc0'], ok['transfer_equal']))
    body = fetch()
    ok['tunnel_after'] = body[0] == marker and body[-1] == '200'
    log('tunnel after update: %s' % body[-1])
    term.close()
    # cleanup
    tun.confirm_delete(target, name)
    client.remove(src)
    target.run([py(target), '-c', 'import urllib.request;urllib.request.urlopen("http://127.0.0.1:%d/__stop")' % port]) if False else None
    failed = [k for k, v in ok.items() if not v]
    print(('FAIL ' + ','.join(failed)) if failed else 'PASS all %s' % ','.join(ok), flush=True)
    sys.exit(1 if failed else 0)


if __name__ == '__main__':
    main()
