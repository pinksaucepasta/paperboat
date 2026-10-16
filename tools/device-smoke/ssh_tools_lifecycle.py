#!/usr/bin/env python3
"""Round-trip a synthetic file tree through the actual managed SSH tools."""
import argparse
import hashlib
import json
from pathlib import Path
import shlex
import tempfile
import uuid

from exec_lifecycle import run
from install_lifecycle import registration


def remote(binary, target, code):
    return run(binary, 'ssh', target, '--', 'python3 -c ' + shlex.quote(code))[0]


def tree_hashes(directory):
    return {str(path.relative_to(directory)): hashlib.sha256(path.read_bytes()).hexdigest()
            for path in directory.rglob('*') if path.is_file()}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    binary = Path.home() / '.local/bin/pb'
    before = registration()
    name = 'smoke-ssh-tools-' + uuid.uuid4().hex
    with tempfile.TemporaryDirectory(prefix=name) as directory:
        root = Path(directory)
        source = root / 'tree'
        (source / 'nested').mkdir(parents=True)
        (source / 'data.bin').write_bytes(bytes(range(256)) * 256)
        (source / 'nested/λ船.txt').write_text('exact Unicode λ船\n')
        (source / 'empty').touch()
        expected = tree_hashes(source)
        setup = ("from pathlib import Path\nimport json\n"
                 "root=Path.home()/'Paperboat'/" + repr(name) + "\n"
                 "root.mkdir(exist_ok=False)\n(root/'scp').mkdir()\n(root/'rsync').mkdir()\n"
                 "print(json.dumps(str(root)))\n")
        destination = json.loads(remote(binary, args.target, setup))
        failure = None
        try:
            run(binary, 'scp', '-r', str(source), args.target + ':' + destination + '/scp/')
            copied = root / 'scp-return'
            run(binary, 'scp', '-r', args.target + ':' + destination + '/scp/tree', str(copied))
            if tree_hashes(copied) != expected:
                raise RuntimeError('managed SCP changed the round-trip tree')
            batch = root / 'sftp.batch'
            returned = root / 'sftp-return.bin'
            batch.write_text('put "' + str(source / 'data.bin') + '" "' + destination + '/sftp.bin"\n'
                             'get "' + destination + '/sftp.bin" "' + str(returned) + '"\n'
                             'rm "' + destination + '/sftp.bin"\n')
            run(binary, 'sftp', '-b', str(batch), args.target)
            if returned.read_bytes() != (source / 'data.bin').read_bytes():
                raise RuntimeError('managed SFTP changed the round-trip bytes')
            run(binary, 'rsync', '-a', str(source) + '/', args.target + ':' + destination + '/rsync/')
            copied = root / 'rsync-return'
            copied.mkdir()
            run(binary, 'rsync', '-a', args.target + ':' + destination + '/rsync/', str(copied) + '/')
            if tree_hashes(copied) != expected:
                raise RuntimeError('managed rsync changed the round-trip tree')
        except Exception as error:
            failure = error
            raise
        finally:
            cleanup = ("from pathlib import Path\nimport shutil\nroot=Path(" + repr(destination) + ")\n"
                       "assert root==Path.home()/'Paperboat'/" + repr(name) + "\nshutil.rmtree(root)\n")
            try:
                remote(binary, args.target, cleanup)
            except Exception as error:
                if failure is None:
                    raise
                failure.add_note('Exact SSH fixture cleanup failed: ' + str(error))
    if registration() != before:
        raise RuntimeError('managed SSH tools changed the enrolled identity')
    print(json.dumps({'check': 'ssh-tools', 'target': args.target, 'status': 'PASS',
                      'tools': ['scp', 'sftp', 'rsync'], 'both_directions': True,
                      'hashes_verified': True, 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
