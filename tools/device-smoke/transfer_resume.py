#!/usr/bin/env python3
"""Interrupt a large batch by restarting the receiver's daemon; every file must arrive once, byte-identical."""
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

FILES = 4
FILE_SIZE = 24 << 20  # 96 MiB batch: about two minutes on the hp-dadape path; per-file limit is 50 MiB
SIZE = FILES * FILE_SIZE
RESTART_AFTER = 25.0
SEND_TIMEOUT = 600


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    binary = Path.home() / '.local/bin/pb'
    before = registration()
    prefix = 'paperboat-smoke-resume-' + uuid.uuid4().hex
    expected = {}
    cleanup = ("from pathlib import Path\n"
               "root=Path.home()/'Paperboat/Inbox'\n"
               "for path in root.glob(" + repr(prefix + '*') + "): path.unlink()\n")
    failure = None
    with tempfile.TemporaryDirectory(prefix='paperboat-resume-') as directory:
        sources = []
        for number in range(FILES):
            path = Path(directory) / ('%s-%d.bin' % (prefix, number))
            digest = hashlib.sha256()
            with path.open('wb') as handle:
                for _ in range(FILE_SIZE >> 20):
                    block = os.urandom(1 << 20)
                    digest.update(block)
                    handle.write(block)
            sources.append(str(path))
            expected[path.name] = digest.hexdigest()
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
            result = subprocess.run([str(binary), 'send', *sources, '--to', args.target, '--json'],
                                    capture_output=True, timeout=SEND_TIMEOUT)
            finished = time.monotonic()
            thread.join(timeout=60)
            if result.returncode:
                try:
                    failure_json = json.loads(result.stdout)['error']
                    detail = '%s %s' % (failure_json.get('code'), failure_json.get('support_reference'))
                except (ValueError, KeyError, TypeError):
                    detail = result.stderr[-200:].decode(errors='replace')
                raise RuntimeError('interrupted send did not finish by itself (exit %d, %.1fs after start): %s' % (result.returncode, finished - started, detail))
            batch = json.loads(result.stdout)
            batch = batch.get('data', batch)
            if len(batch['transfers']) != FILES:
                raise RuntimeError('resumed send did not report every file')
            if 'at' not in restarted or finished - restarted['at'] < 2 or restarted['at'] - started < RESTART_AFTER - 1:
                raise RuntimeError('the transfer was not still running when the receiver restarted')
            verify = ("from pathlib import Path\nimport hashlib\n"
                      "root=Path.home()/'Paperboat/Inbox'\n"
                      "expected=" + repr(expected) + "\n"
                      "files=sorted(root.glob(" + repr(prefix + '*') + "))\n"
                      "assert [f.name for f in files]==sorted(expected),[f.name for f in files]\n"
                      "for f in files:\n"
                      " assert f.stat().st_size==" + str(FILE_SIZE) + "\n"
                      " h=hashlib.sha256()\n"
                      " with f.open('rb') as fh:\n"
                      "  for b in iter(lambda:fh.read(1<<20),b''): h.update(b)\n"
                      " assert h.hexdigest()==expected[f.name],f.name\n"
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
