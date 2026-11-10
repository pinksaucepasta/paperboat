#!/usr/bin/env python3
"""F14-02: during an update, a running file transfer finishes byte-identical and a preview and a tunnel come back on the same URL.

Usage: update_survival.py TARGET PEER (--approve | --from FILE) [--terminal] --mib 600 (sudo password on stdin).
With --terminal a shell is also held open in a real PTY (`pb connect`) and, after the update, the SAME shell must answer.
Runs on TARGET (the machine that is updated). PEER is a Unix machine of the same account that receives;
TARGET sends two files to PEER. Use --mib 600 so the transfer is still running when the update starts
(`pb update --approve <downloaded candidate> --force`, or `pb update --from FILE --force`).
Afterwards: the sender exits 0 and both files in PEER's Inbox match the sender's SHA-256 digests;
the preview URL and the tunnel URL are unchanged and serve their marker again (a gap while the runtime restarts is allowed).
"""
import argparse
import contextlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
import uuid

from install_lifecycle import registration
from pairing_lifecycle import command_result, invoke
from preview_lifecycle import stoppable_origin
from preview_receipt import PREVIEW_COMMAND_TIMEOUT, receipt_value
from session_cleanup import cleanup
from terminal_lifecycle import Terminal, wait_windows_shell
from update_continuity import SHELL_PID, windows_pid
import windows_target
import winhost

PB = winhost.pb_path()
FILES = 2
FILE_SIZE = 40 << 20  # default; --mib overrides
GAP_SECONDS = 240
POLL_SECONDS = 60
STALL_SECONDS = 300
CAP_SECONDS = 90 * 60


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


def committed(target, prefix):
    """Bytes the receiver has committed for this run's files (its durable offset, the one a resumed send continues from)."""
    page = invoke(PB, 'send', 'list', '--on', target, '--q', prefix, '--json', timeout=60)
    return sum(item.get('committed_offset', 0) for item in page['items'] if item['basename'].startswith(prefix))


def wait_sender(sender, progress, clock=time.monotonic):
    """Wait while the receiver's committed bytes keep growing: a slow path (a hosted runner with a 250-800 ms RTT moves
    under 1 MiB/s) is not a hung transfer. Fails on STALL_SECONDS without progress, or after CAP_SECONDS in all."""
    started = last_change = clock()
    seen = -1
    while True:
        try:
            return sender.wait(POLL_SECONDS)
        except subprocess.TimeoutExpired:
            pass
        try:
            now = progress()
        except RuntimeError:  # a failed listing is no progress
            now = seen
        if now > seen:
            seen, last_change = now, clock()
        stalled = clock() - last_change >= STALL_SECONDS
        if stalled or clock() - started >= CAP_SECONDS:
            text = (sender.stdout() + sender.stderr()).decode(errors='replace')[-2000:]  # read before the kill removes the job
            sender.kill()
            what = 'made no progress for %d s' % STALL_SECONDS if stalled else 'did not finish within %d s' % CAP_SECONDS
            raise RuntimeError('the transfer %s after the update (receiver committed %d bytes); sender output: %s'
                               % (what, max(seen, 0), text or 'none'))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('peer')
    parser.add_argument('--mib', type=int, default=40, help='size of each of the two transferred files')
    parser.add_argument('--transfer-only', action='store_true', help='skip the preview and tunnel checks')
    parser.add_argument('--terminal', action='store_true', help='hold a terminal on TARGET across the update; the same shell must answer after it')
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument('--approve', action='store_true')
    mode.add_argument('--from', dest='source')
    args = parser.parse_args()
    global FILE_SIZE
    FILE_SIZE = args.mib << 20
    password = None if winhost.device() else sys.stdin.readline().rstrip('\n')
    windows = windows_target.is_windows(PB, args.target)
    before = registration()
    marker = 'PB_UPDATE_SURVIVE_' + uuid.uuid4().hex
    name = 'smoke-usurvive-' + uuid.uuid4().hex[:8]
    prefix = 'paperboat-usurvive-' + uuid.uuid4().hex[:10]
    preview_id = tunnel_id = None
    held = None
    terminal_error = ''
    session_name = 'smoke-usurvive-term-' + uuid.uuid4().hex[:8]
    result = {'check': 'update-survival', 'target': args.target, 'peer': args.peer}
    error = None

    def shell_identity(label):
        """Ask the held shell for its process id: $$ in sh; under cmd.exe the parent of a PowerShell typed there."""
        if windows:
            held.send(windows_target.typed_marker('PB_' + label + '_', SHELL_PID) + '\r')
        else:
            held.send("printf '\\nPB_" + label + "''_%s\\n' \"$$\"\r")  # the typed echo must not contain the marker
        held.expect('PB_' + label + '_', timeout=180)
        return windows_pid(held) if windows else held.expect('\n', timeout=10).decode().strip()
    with tempfile.TemporaryDirectory(prefix='paperboat-update-survive-') as directory, \
            winhost.workdir('paperboat-update-survive-') as files, contextlib.ExitStack() as stack:
        (Path(directory) / 'index.html').write_text('<p>' + marker + '</p>')
        # On the Windows device the origin runs there: an ssh -R forward from this controller would depend on the device's
        # OpenSSH transport, which the update churns (the line-166 failure on winpool-11), and is not what is being judged.
        port, stop_origin = stoppable_origin(directory, '<p>' + marker + '</p>')
        sender = None
        try:
            if not args.transfer_only:
                lease = invoke(PB, 'preview', str(port), '--background', '--ttl', '40m', '--json', timeout=PREVIEW_COMMAND_TIMEOUT)
                preview_id, preview_url = lease['id'], receipt_value(lease, 'endpoint')
                created = tunnel('create', name, '--port', str(port), '--duration', '40m', '--wait', '--timeout', '90s')
                tunnel_id, tunnel_url = created['tunnel']['id'], created['tunnel']['stable_endpoint']
                wait_for(lambda: marker in body(preview_url), 'the preview to serve', 120)
                wait_for(lambda: marker in body(tunnel_url), 'the tunnel to serve', 120)
            if args.terminal:
                terminal_args = ['connect', args.target, '--name', session_name, '--status-bar', 'off']
                held = Terminal(winhost.terminal_argv(PB, terminal_args), env=winhost.terminal_env(PB))
                if not windows:
                    held.expect(b'\x1b[?2004h')
                    held.expect(held.marker('READY', 'stty -echo; '))
                else:  # no bracketed paste under cmd.exe: wait for a printed marker, not for a quiet pty (typed bytes before attach are swallowed)
                    wait_windows_shell(held)
                shell_pid = shell_identity('SHELL')
                if not shell_pid.isdigit():
                    raise RuntimeError('could not read the held shell identity')
            # TARGET creates two random files and starts sending them to PEER; the update restarts TARGET's runtime meanwhile.
            hashes = {}
            send_files = []
            for number in range(FILES):
                path = files / ('%s-%d.bin' % (prefix, number))
                hashes[path.name] = winhost.write_random(path, FILE_SIZE)
                send_files.append(str(path))
            sender = winhost.job(PB, ['send', *send_files, '--to', args.peer, '--json'])
            time.sleep(10)
            if sender.poll() is not None:
                raise RuntimeError('the transfer ended before the update started: ' + sender.stdout().decode()[-200:])
            if args.approve:
                status = json.loads(command_result(PB, 'update', 'status', '--json', timeout=60).stdout)['data']
                candidate = status['candidate']['id']
                update = ['update', '--approve', candidate, '--force', '--json']
            else:
                update = ['update', '--from', args.source, '--force', '--json']
            update_result = command_result(PB, *update, terminal=True, password=password, timeout=900)
            text = re.sub(r'\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x1b]*\x1b\\', '', update_result.stdout)
            if update_result.returncode:
                raise RuntimeError('update failed rc=%s: %s' % (update_result.returncode, text[-400:]))
            result['update_started'] = True
            if held is not None:
                # judged with the rest: a lost shell must not hide whether the preview, tunnel and transfer survived
                if shell_identity('AGAIN') == shell_pid:
                    result['same_shell_after_update'] = True
                else:
                    terminal_error = 'the held terminal reconnected to a different shell after the update'
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
            code = wait_sender(sender, lambda: committed(args.peer, prefix))
            if code != 0:
                raise RuntimeError('the transfer failed after the update (rc %s): %s' % (code, (sender.stdout() + sender.stderr()).decode()[-2000:]))
            listing = peer(args.peer, 'cd ~/Paperboat/Inbox && sha256sum %s-*.bin' % prefix, 120).stdout
            received = {line.split()[1].lstrip('*'): line.split()[0] for line in listing.splitlines() if len(line.split()) == 2}
            if received != hashes:
                raise RuntimeError('received files differ from the sent ones: %r vs %r' % (sorted(received), sorted(hashes)))
            result['transfer_byte_identical'] = True
            if terminal_error:
                raise RuntimeError(terminal_error + ' (preview, tunnel and transfer survived: %s)' % {k: v for k, v in result.items() if k.endswith(('same_url', 'identical'))})
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
            if held is not None:
                try:
                    held.send('exit\r')
                    held.wait_exit(30)
                except BaseException:
                    pass
                held.close()
                try:
                    cleanup(PB, args.target, session_name)
                except BaseException:
                    pass
            if sender is not None:
                if sender.poll() is None:
                    sender.kill()
                sender.close()
            stop_origin()
    assert registration() == before, 'enrollment changed'
    result['status'] = 'PASS'
    print(json.dumps(result), flush=True)


if __name__ == '__main__':
    main()
