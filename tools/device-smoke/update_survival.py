#!/usr/bin/env python3
"""F14-02: during an update, a running file transfer finishes byte-identical and a preview and a tunnel come back on the same URL.

Usage: update_survival.py TARGET PEER (--approve | --from FILE) (sudo password on stdin).
Runs on TARGET (the machine that is updated). PEER is another machine of the same account that receives;
TARGET sends two 40 MiB files to PEER. While the transfer runs, the update is started
(`pb update --approve <downloaded candidate> --force`, or `pb update --from FILE --force`).
Afterwards: the sender exits 0 and both files in PEER's Inbox match the sender's SHA-256 digests;
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
import subprocess
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
FILE_SIZE = 40 << 20  # default; --mib overrides
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
    parser.add_argument('--mib', type=int, default=40, help='size of each of the two transferred files')
    parser.add_argument('--transfer-only', action='store_true', help='skip the preview and tunnel checks')
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument('--approve', action='store_true')
    mode.add_argument('--from', dest='source')
    args = parser.parse_args()
    global FILE_SIZE
    FILE_SIZE = args.mib << 20
    password = sys.stdin.readline().rstrip('\n')
    before = registration()
    marker = 'PB_UPDATE_SURVIVE_' + uuid.uuid4().hex
    name = 'smoke-usurvive-' + uuid.uuid4().hex[:8]
    prefix = 'paperboat-usurvive-' + uuid.uuid4().hex[:10]
    preview_id = tunnel_id = None
    result = {'check': 'update-survival', 'target': args.target, 'peer': args.peer}
    error = None
    with tempfile.TemporaryDirectory(prefix='paperboat-update-survive-') as directory:
        (Path(directory) / 'index.html').write_text('<p>' + marker + '</p>')
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), functools.partial(QuietHandler, directory=directory))
        threading.Thread(target=server.serve_forever, daemon=True).start()
        port = server.server_port
        try:
            if not args.transfer_only:
                lease = invoke(PB, 'preview', str(port), '--background', '--ttl', '40m', '--json', timeout=PREVIEW_COMMAND_TIMEOUT)
                preview_id, preview_url = lease['id'], receipt_value(lease, 'endpoint')
                created = tunnel('create', name, '--port', str(port), '--duration', '40m', '--wait', '--timeout', '90s')
                tunnel_id, tunnel_url = created['tunnel']['id'], created['tunnel']['stable_endpoint']
                wait_for(lambda: marker in body(preview_url), 'the preview to serve', 120)
                wait_for(lambda: marker in body(tunnel_url), 'the tunnel to serve', 120)
            # TARGET creates two random files and starts sending them to PEER; the update restarts TARGET's runtime meanwhile.
            hashes = {}
            send_files = []
            for number in range(FILES):
                path = Path(directory) / ('%s-%d.bin' % (prefix, number))
                digest = hashlib.sha256()
                with path.open('wb') as handle:
                    for _ in range(FILE_SIZE >> 20):
                        block = os.urandom(1 << 20)
                        digest.update(block)
                        handle.write(block)
                hashes[path.name] = digest.hexdigest()
                send_files.append(str(path))
            sender = subprocess.Popen([str(PB), 'send', *send_files, '--to', args.peer, '--json'],
                                      stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            time.sleep(10)
            if sender.poll() is not None:
                raise RuntimeError('the transfer ended before the update started: ' + sender.communicate()[0].decode()[-200:])
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
            if not args.transfer_only:
                wait_for(lambda: marker in body(preview_url), 'the preview URL to serve again after the update')
                result['preview_same_url'] = True
                wait_for(lambda: marker in body(tunnel_url), 'the tunnel URL to serve again after the update')
                result['tunnel_same_url'] = True
                def ready_endpoint():
                    try:
                        return receipt_value(invoke(PB, 'preview', 'status', preview_id, '--json', timeout=30), 'endpoint')
                    except (ValueError, KeyError, RuntimeError):
                        return None
                wait_for(lambda: ready_endpoint() == preview_url, 'the preview lease to report ready on the same URL')
                wait_for(lambda: tunnel('status', tunnel_id).get('overall_code') == 'ready', 'the tunnel to report ready (same stable URL already served)')
            try:
                out, errs = sender.communicate(timeout=600)
            except subprocess.TimeoutExpired:
                sender.kill()
                raise RuntimeError('the transfer did not finish within 10 minutes of the update')
            if sender.returncode != 0:
                raise RuntimeError('the transfer failed after the update (rc %s): %s' % (sender.returncode, (out + errs).decode()[-300:]))
            listing = peer(args.peer, 'cd ~/Paperboat/Inbox && sha256sum %s-*.bin' % prefix, 120).stdout
            received = {line.split()[1].lstrip('*'): line.split()[0] for line in listing.splitlines() if len(line.split()) == 2}
            if received != hashes:
                raise RuntimeError('received files differ from the sent ones: %r vs %r' % (sorted(received), sorted(hashes)))
            result['transfer_byte_identical'] = True
        except BaseException as caught:
            error = caught
            raise
        finally:
            for step in (
                lambda: invoke(PB, 'preview', 'stop', preview_id, '--json', timeout=60) if preview_id else None,
                lambda: peer(args.peer, 'rm -f ~/Paperboat/Inbox/%s-*.bin' % prefix, 60),
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
