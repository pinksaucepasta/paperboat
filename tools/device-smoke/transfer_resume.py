#!/usr/bin/env python3
"""Interrupt a large transfer by restarting the receiver's daemon; it must finish, byte-identical, once."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import time
import uuid

from exec_lifecycle import run
from install_lifecycle import registration

SIZE = 384 << 20
RESTART_AFTER = 6.0
SEND_TIMEOUT = 900


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    binary = Path.home() / '.local/bin/pb'
    before = registration()
    prefix = 'paperboat-smoke-resume-' + uuid.uuid4().hex
    name = prefix + '.bin'
    digest = hashlib.sha256()
    cleanup = ("from pathlib import Path\n"
               "root=Path.home()/'Paperboat/Inbox'\n"
               "for path in root.glob(" + repr(prefix + '*') + "): path.unlink()\n")
    failure = None
    with tempfile.TemporaryDirectory(prefix='paperboat-resume-') as directory:
        source = Path(directory) / name
        with source.open('wb') as handle:
            for _ in range(SIZE >> 20):
                block = os.urandom(1 << 20)
                digest.update(block)
                handle.write(block)
        expected = digest.hexdigest()
        restarted = {}

        def interrupt():
            time.sleep(RESTART_AFTER)
            command = '(sleep 1; ~/.local/bin/pb service restart) >/dev/null 2>&1 &'
            run(binary, 'exec', args.target, '--', 'sh', '-c', command)
            restarted['at'] = time.monotonic()

        try:
            thread = threading.Thread(target=interrupt, daemon=True)
            started = time.monotonic()
            thread.start()
            result = subprocess.run([str(binary), 'send', str(source), '--to', args.target, '--json'],
                                    capture_output=True, timeout=SEND_TIMEOUT)
            finished = time.monotonic()
            thread.join(timeout=60)
            if result.returncode:
                raise RuntimeError('interrupted send did not finish by itself (exit %d)' % result.returncode)
            batch = json.loads(result.stdout)
            batch = batch.get('data', batch)
            if len(batch['transfers']) != 1:
                raise RuntimeError('resumed send did not report exactly one transfer')
            if 'at' not in restarted or finished - restarted['at'] < 2 or restarted['at'] - started < RESTART_AFTER - 1:
                raise RuntimeError('the transfer was not still running when the receiver restarted')
            verify = ("from pathlib import Path\nimport hashlib\n"
                      "root=Path.home()/'Paperboat/Inbox'\n"
                      "files=sorted(root.glob(" + repr(prefix + '*') + "))\n"
                      "assert [f.name for f in files]==[" + repr(name) + "],[f.name for f in files]\n"
                      "assert files[0].stat().st_size==" + str(SIZE) + "\n"
                      "h=hashlib.sha256()\n"
                      "with files[0].open('rb') as f:\n"
                      " for b in iter(lambda:f.read(1<<20),b''): h.update(b)\n"
                      "assert h.hexdigest()==" + repr(expected) + "\n"
                      "print('PB_RESUME_VERIFIED')\n")
            output, errors = run(binary, 'exec', args.target, '--', 'python3', '-c', verify)
            if output != b'PB_RESUME_VERIFIED\n' or errors:
                raise RuntimeError('received file was not verified')
        except Exception as error:
            failure = error
            raise
        finally:
            try:
                run(binary, 'exec', args.target, '--', 'python3', '-c', cleanup)
            except Exception as error:
                if failure is None:
                    raise
                failure.add_note('Exact received-file cleanup failed: ' + str(error))
    if registration() != before:
        raise RuntimeError('transfer resume changed the enrolled identity')
    print(json.dumps({'check': 'transfer-resume', 'target': args.target, 'status': 'PASS',
                      'bytes': SIZE, 'interrupted_after_s': RESTART_AFTER, 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
