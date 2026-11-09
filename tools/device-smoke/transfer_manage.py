#!/usr/bin/env python3
"""Verify public transfer history, active cancellation and destination settings."""
import argparse
import hashlib
import json
import time
import uuid

from exec_lifecycle import run
from install_lifecycle import registration
from pairing_lifecycle import invoke
from ssh_tools_lifecycle import remote
import windows_target
import winhost


def call(binary, *arguments):
    return invoke(binary, *arguments, '--json', timeout=20)


def cancel_upload(binary, target, path):
    identity = None
    stopped = False
    failure = None
    process = winhost.job(binary, ['send', str(path), '--to', target, '--json'])
    try:
        deadline = time.monotonic() + 45
        while time.monotonic() < deadline:
            page = call(binary, 'send', 'list', '--on', target, '--q', path.name)
            items = [item for item in page['items'] if item['basename'] == path.name]
            if items:
                if len(items) != 1 or items[0]['state'] not in ['created', 'uploading', 'pending']:
                    raise RuntimeError('cancel fixture was not one active transfer')
                identity = items[0]['transfer_id']
                process.suspend()
                stopped = True
                break
            if process.poll() is not None:
                raise RuntimeError('send ended before its active transfer could be canceled')
            time.sleep(.2)
        else:
            raise RuntimeError('active transfer was not visible within45s')
        canceled = call(binary, 'send', 'cancel', identity, '--on', target)
        if canceled['transfer_id'] != identity or canceled['state'] != 'canceled':
            raise RuntimeError('cancel did not identify its exact transfer')
        status = call(binary, 'send', 'status', identity, '--on', target)
        if status['state'] != 'canceled':
            raise RuntimeError('receiver did not retain canceled transfer state')
        process.resume()
        stopped = False
        if process.wait(30) == 0:
            raise RuntimeError('canceled send reported successful delivery')
    except Exception as error:
        failure = error
        raise
    finally:
        if identity is not None and failure is not None:
            try:
                call(binary, 'send', 'cancel', identity, '--on', target)
            except Exception as cleanup_error:
                failure.add_note('Exact active transfer cleanup failed: ' + str(cleanup_error))
        if stopped and process.poll() is None:
            process.resume()
        if process.poll() is None:
            process.kill()
        process.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    binary = winhost.pb_path()
    enrolled = registration()
    before = call(binary, 'send', 'destination')
    name = 'paperboat-send-manage-' + uuid.uuid4().hex
    failure = None
    with winhost.workdir(name) as root:
        small, large = root / (name + '.txt'), root / (name + '-cancel.bin')
        small.write_bytes(b'exact-transfer-settings-fixture')
        winhost.write_pattern(large, bytes(range(256)) * 256, 800)  # 50 MiB
        try:
            selected = call(binary, 'send', 'destination', 'set', args.target)
            observed = call(binary, 'send', 'destination')
            if not selected['configured'] or observed['machine']['id'] != selected['machine']['id']:
                raise RuntimeError('transfer destination did not persist')
            call(binary, 'send', 'destination', 'clear')
            if call(binary, 'send', 'destination')['configured']:
                raise RuntimeError('transfer destination did not clear')
            call(binary, 'send', 'destination', 'set', args.target)
            batch = call(binary, 'send', str(small))
            identity = batch['transfers'][0]['transfer_id']
            status = call(binary, 'send', 'status', identity, '--on', args.target)
            if status['state'] != 'published' or status['sha256'] != hashlib.sha256(small.read_bytes()).hexdigest():
                raise RuntimeError('transfer status lost its published hash')
            page = call(binary, 'send', 'list', '--on', args.target, '--q', small.name)
            if [item['transfer_id'] for item in page['items']] != [identity]:
                raise RuntimeError('transfer history lost its exact published transfer')
            cancel_upload(binary, args.target, large)
        except Exception as error:
            failure = error
            raise
        finally:
            try:
                if before['configured']:
                    call(binary, 'send', 'destination', 'set', before['machine']['id'])
                else:
                    call(binary, 'send', 'destination', 'clear')
                cleanup = ("from pathlib import Path\nroot=Path.home()/'Paperboat/Inbox'\n"
                           "names=" + repr([small.name, large.name]) + "\n"
                           "for name in names:\n assert name.startswith(" + repr(name) + ")\n (root/name).unlink(missing_ok=True)\n")
                if windows_target.is_windows(binary, args.target):
                    run(binary, 'exec', args.target, '--', *windows_target.ps_argv(
                        windows_target.inbox_cleanup_script(name, [small.name, large.name])))
                else:
                    remote(binary, args.target, cleanup)
            except Exception as error:
                if failure is None:
                    raise
                failure.add_note('Transfer fixture restoration failed: ' + str(error))
    if registration() != enrolled:
        raise RuntimeError('transfer management changed the enrolled identity')
    print(json.dumps({'check': 'transfer-manage', 'target': args.target, 'status': 'PASS',
                      'phases': ['list', 'status', 'cancel', 'destination-set-clear'],
                      'settings_restored': True, 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
