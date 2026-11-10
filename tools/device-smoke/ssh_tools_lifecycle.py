#!/usr/bin/env python3
"""Round-trip a synthetic file tree through the actual managed SSH tools."""
import argparse
import hashlib
import json
import shlex
import uuid

from exec_lifecycle import run
from install_lifecycle import registration
import windows_target
import winhost


def remote(binary, target, code):
    return run(binary, 'ssh', target, '--', 'python3 -c ' + shlex.quote(code))[0]


def tree_hashes(directory):
    if isinstance(directory, winhost.RemotePath):
        return directory.tree_hashes()
    return {str(path.relative_to(directory)): hashlib.sha256(path.read_bytes()).hexdigest()
            for path in directory.rglob('*') if path.is_file()}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--tools', nargs='+', choices=['scp', 'sftp', 'rsync'],
                        help='tools to qualify; rsync must be installed on both endpoints')
    args = parser.parse_args()
    binary = winhost.pb_path()
    before = registration()
    name = 'smoke-ssh-tools-' + uuid.uuid4().hex
    windows = windows_target.is_windows(binary, args.target)
    # Native Windows OpenSSH ships scp/sftp. An installed rsync is qualified by
    # selecting it explicitly, with the same round-trip hash checks as Unix.
    tools = args.tools or (['scp', 'sftp'] if windows or winhost.device() else ['scp', 'sftp', 'rsync'])
    with winhost.workdir(name) as root:
        source = root / 'tree'
        (source / 'nested').mkdir(parents=True)
        (source / 'data.bin').write_bytes(bytes(range(256)) * 256)
        (source / 'nested/λ船.txt').write_text('exact Unicode λ船\n')
        (source / 'empty').write_bytes(b'')
        expected = tree_hashes(source)
        setup = ("from pathlib import Path\nimport json\n"
                 "root=Path.home()/'Paperboat'/" + repr(name) + "\n"
                 "root.mkdir(exist_ok=False)\n(root/'scp').mkdir()\n(root/'rsync').mkdir()\n"
                 "print(json.dumps(str(root)))\n")
        if windows:  # no python3 there; the directory is named relative to the home scp and sftp start in
            run(binary, 'ssh', args.target, '--', windows_target.ssh_command(windows_target.tools_setup_script(name)))
            destination = 'Paperboat/' + name
        else:
            destination = json.loads(remote(binary, args.target, setup))
        failure = None
        try:
            if 'scp' in tools:
                run(binary, 'scp', '-r', str(source), args.target + ':' + destination + '/scp/')
                copied = root / 'scp-return'
                run(binary, 'scp', '-r', args.target + ':' + destination + '/scp/tree', str(copied))
                if tree_hashes(copied) != expected:
                    raise RuntimeError('managed SCP changed the round-trip tree')
            if 'sftp' in tools:
                batch = root / 'sftp.batch'
                returned = root / 'sftp-return.bin'
                # sftp's batch paths escape backslashes; Windows accepts forward slashes.
                batch.write_text('put "' + str(source / 'data.bin').replace('\\', '/') + '" "' + destination + '/sftp.bin"\n'
                                 'get "' + destination + '/sftp.bin" "' + str(returned).replace('\\', '/') + '"\n'
                                 'rm "' + destination + '/sftp.bin"\n')
                run(binary, 'sftp', '-b', str(batch), args.target)
                if returned.read_bytes() != (source / 'data.bin').read_bytes():
                    raise RuntimeError('managed SFTP changed the round-trip bytes')
            if 'rsync' in tools:
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
                if windows:
                    run(binary, 'ssh', args.target, '--', windows_target.ssh_command(windows_target.tools_cleanup_script(name)))
                else:
                    remote(binary, args.target, cleanup)
            except Exception as error:
                if failure is None:
                    raise
                failure.add_note('Exact SSH fixture cleanup failed: ' + str(error))
    if registration() != before:
        raise RuntimeError('managed SSH tools changed the enrolled identity')
    print(json.dumps({'check': 'ssh-tools', 'target': args.target, 'status': 'PASS',
                      'tools': tools, 'both_directions': True,
                      'hashes_verified': True, 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
