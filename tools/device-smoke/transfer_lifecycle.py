#!/usr/bin/env python3
"""Send a real multi-file batch and verify the received bytes independently."""
import argparse
import hashlib
import json
from pathlib import Path
import tempfile
import uuid

from exec_lifecycle import run
from install_lifecycle import registration
from pairing_lifecycle import invoke


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    binary = Path.home() / '.local/bin/pb'
    before = registration()
    prefix = 'paperboat-smoke-' + uuid.uuid4().hex
    expected = {}
    names = []
    with tempfile.TemporaryDirectory(prefix=prefix) as directory:
        for suffix, content in [('large.bin', bytes(range(256)) * 204800),
                                ('unicode-λ船.txt', 'λ船\n'.encode() * 4096),
                                ('empty.bin', b'')]:
            path = Path(directory) / (prefix + '-' + suffix)
            path.write_bytes(content)
            names.append(str(path))
            expected[path.name] = hashlib.sha256(content).hexdigest()
        # Receiver verification and cleanup use only our exact random filenames.
        cleanup = ("from pathlib import Path\n"
                   "root=Path.home()/'Paperboat/Inbox'\n"
                   "names=" + repr(list(expected)) + "\n"
                   "for name in names:\n"
                   " assert name.startswith(" + repr(prefix + '-') + ") and Path(name).name==name\n"
                   " (root/name).unlink(missing_ok=True)\n")
        failure = None
        try:
            batch = invoke(binary, 'send', *names, '--to', args.target, '--json', timeout=180)
            if len(batch['transfers']) != len(expected):
                raise RuntimeError('multi-file batch lost a transfer receipt')
            verify = ("from pathlib import Path\nimport hashlib,json\n"
                      "root=Path.home()/'Paperboat/Inbox'\n"
                      "expected=" + repr(expected) + "\n"
                      "assert all(hashlib.sha256((root/name).read_bytes()).hexdigest()==digest for name,digest in expected.items())\n"
                      "print('PB_MULTI_FILE_VERIFIED')\n")
            output, errors = run(binary, 'exec', args.target, '--', 'python3', '-c', verify)
            if output != b'PB_MULTI_FILE_VERIFIED\n' or errors:
                raise RuntimeError('received multi-file hashes were not verified')
        except Exception as error:
            failure = error
            raise
        finally:
            try:
                run(binary, 'exec', args.target, '--', 'python3', '-c', cleanup)
            except Exception as error:
                if failure is None:
                    raise
                failure.add_note('Exact receiver cleanup failed: ' + str(error))
    if registration() != before:
        raise RuntimeError('transfer changed the enrolled identity')
    print(json.dumps({'check': 'transfer-multi', 'target': args.target, 'status': 'PASS',
                      'files': 3, 'largest_bytes': 50 << 20, 'hashes_verified': True,
                      'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
