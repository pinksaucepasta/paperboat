#!/usr/bin/env python3
"""F14-02: during an update, a running file transfer finishes byte-identical and a preview and a tunnel come back on the same URL.

Usage: update_survival.py TARGET PEER (--approve | --from FILE) (sudo password on stdin).
Runs on TARGET (the machine that is updated). PEER is another machine of the same account whose
`pb` sends two 40 MiB files to TARGET. While the transfer runs, the update is started
(`pb update --approve <downloaded candidate> --force`, or `pb update --from FILE --force`).
Afterwards: the sender exits 0 and both files in TARGET's Inbox match the sender's SHA-256 digests;
the preview URL and the tunnel URL are unchanged and serve their marker again (a gap while the runtime restarts is allowed).
"""
import argparse
import functools
import hashlib
import http.server
import json
import os
from pathlib import Path
import re
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
import uuid

from install_lifecycle import registration
from pairing_lifecycle import command_result, invoke
from preview_lifecycle import QuietHandler
from preview_receipt import PREVIEW_COMMAND_TIMEOUT, receipt_value
import terminal_command

PB = Path.home() / '.local/bin/pb'
FILES = 2
FILE_SIZE = 40 << 20
GAP_SECONDS = 240


def body(url):
    try:
        return urllib.request.urlopen(url + '/', timeout=15).read().decode('utf-8', 'replace')
    except urllib.error.HTTPError as error:
        return 'HTTP %d' % error.code
    except OSError:
        return ''


def wait_for(check, what, seconds=GAP_SECONDS):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if check():
            return
        time.sleep(3)
    raise RuntimeError('timed out waiting for ' + what)


def tunnel(*args):
    result = command_result(PB, 'tunnel', *args, '--json', timeout=240)
    value = json.loads(result.stdout)
    if result.returncode or (isinstance(value, dict) and value.get('ok') is False):
        raise RuntimeError('tunnel ' + ' '.join(args[:2]) + ' failed')
    return value.get('data', value) if isinstance(value, dict) and 'ok' in value else value


def peer(target, command, timeout=120):
    return command_result(PB, 'exec', target, '--', 'sh', '-c', command, timeout=timeout)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('peer')
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument('--approve', action='store_true')
    mode.add_argument('--from', dest='source')
    args = parser.parse_args()
    password = sys.stdin.readline().rstrip('\n')
    before = registration()
    marker = 'PB_UPDATE_SURVIVE_' + uuid.uuid4().hex
    name = 'smoke-usurvive-' + uuid.uuid4().hex[:8]
    prefix = 'paperboat-usurvive-' + uuid.uuid4().hex[:10]
    workdir = '/tmp/' + prefix
    preview_id = tunnel_id = None
    result = {'check': 'update-survival', 'target': args.target, 'peer': args.peer}
    error = None
    with tempfile.TemporaryDirectory(prefix='paperboat-update-survive-') as directory:
        (Path(directory) / 'index.html').write_text('<p>' + marker + '</p>')
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), functools.partial(QuietHandler, directory=directory))
        threading.Thread(target=server.serve_forever, daemon=True).start()
        port = server.server_port
        try:
            lease = invoke(PB, 'preview', str(port), '--background', '--ttl', '40m', '--json', timeout=PREVIEW_COMMAND_TIMEOUT)
            preview_id, preview_url = lease['id'], receipt_value(lease, 'endpoint')
            created = tunnel('create', name, '--port', str(port), '--duration', '40m', '--wait', '--timeout', '90s')
            tunnel_id, tunnel_url = created['tunnel']['id'], created['tunnel']['stable_endpoint']
            wait_for(lambda: marker in body(preview_url), 'the preview to serve', 120)
            wait_for(lambda: marker in body(tunnel_url), 'the tunnel to serve', 120)
            # Peer creates the files and starts the transfer in the background.
            make = ('set -e; mkdir -p {d}; cd {d}; for i in 0 1; do head -c {n} /dev/urandom > {p}-$i.bin; done; '
                    'sha256sum {p}-*.bin | cut -d" " -f1,3 > sums.txt; '
                    '(nohup sh -c "~/.local/bin/pb send {p}-0.bin {p}-1.bin --to {t} --json > send.out 2>&1; echo $? > send.rc" >/dev/null 2>&1 &); sleep 1; echo STARTED'
                    ).format(d=workdir, n=FILE_SIZE, p=prefix, t=args.target)
            started = peer(args.peer, make)
            if 'STARTED' not in started.stdout:
                raise RuntimeError('the peer could not start the transfer: ' + started.stdout[-200:])
            time.sleep(10)
            if args.approve:
                status = json.loads(command_result(PB, 'update', 'status', '--json', timeout=60).stdout)['data']
                candidate = status['candidate']['id']
                update = [str(PB), 'update', '--approve', candidate, '--force', '--json']
            else:
                update = [str(PB), 'update', '--from', args.source, '--force', '--json']
            update_result = terminal_command.run(update, password=password, timeout=900)
            text = re.sub(r'\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x1b]*\x1b\\', '', update_result.stdout)
            if update_result.returncode:
                raise RuntimeError('update failed rc=%s: %s' % (update_result.returncode, text[-400:]))
            result['update_started'] = True
            wait_for(lambda: marker in body(preview_url), 'the preview URL to serve again after the update')
            result['preview_same_url'] = True
            wait_for(lambda: marker in body(tunnel_url), 'the tunnel URL to serve again after the update')
            result['tunnel_same_url'] = True
            wait_for(lambda: invoke(PB, 'preview', 'status', preview_id, '--json', timeout=30)['state'] == 'ready',
                     'the preview lease to report ready again')
            after_preview = invoke(PB, 'preview', 'status', preview_id, '--json', timeout=30)
            assert receipt_value(after_preview, 'endpoint') == preview_url, 'preview URL changed'
            assert tunnel('status', tunnel_id)['tunnel']['stable_endpoint'] == tunnel_url, 'tunnel URL changed'
            deadline = time.monotonic() + 600
            while time.monotonic() < deadline:
                done = peer(args.peer, 'cat %s/send.rc 2>/dev/null' % workdir, 60).stdout.strip()
                if done:
                    break
                time.sleep(5)
            else:
                raise RuntimeError('the transfer did not finish within 10 minutes of the update')
            if done != '0':
                raise RuntimeError('the transfer failed after the update (rc %s): %s' % (
                    done, peer(args.peer, 'tail -c 300 %s/send.out' % workdir, 60).stdout))
            sums = dict(line.split()[::-1] for line in peer(args.peer, 'cat %s/sums.txt' % workdir, 60).stdout.splitlines() if line.strip())
            inbox = Path.home() / 'Paperboat/Inbox'
            for file_name, digest in sums.items():
                file_name = Path(file_name).name
                received = inbox / file_name
                actual = hashlib.sha256(received.read_bytes()).hexdigest()
                if actual != digest.lstrip('*'):
                    raise RuntimeError('received file differs: ' + file_name)
            result['transfer_byte_identical'] = len(sums) == FILES
            if len(sums) != FILES:
                raise RuntimeError('not every file arrived')
        except BaseException as caught:
            error = caught
            raise
        finally:
            for step in (
                lambda: invoke(PB, 'preview', 'stop', preview_id, '--json', timeout=60) if preview_id else None,
                lambda: peer(args.peer, 'rm -rf %s' % workdir, 60),
                lambda: [p.unlink() for p in (Path.home() / 'Paperboat/Inbox').glob(prefix + '*')],
            ):
                try:
                    step()
                except BaseException:
                    if error is None:
                        raise
            try:
                if tunnel_id:
                    token = json.loads(command_result(PB, 'tunnel', 'delete', tunnel_id, '--json', timeout=120).stdout)
                    token = (token.get('data') or token).get('confirmation_token')
                    if token:
                        command_result(PB, 'tunnel', 'delete', tunnel_id, '--json', '--confirm', token, timeout=120)
            except BaseException:
                pass
            server.shutdown()
            server.server_close()
    assert registration() == before, 'enrollment changed'
    result['status'] = 'PASS'
    print(json.dumps(result), flush=True)


if __name__ == '__main__':
    main()
