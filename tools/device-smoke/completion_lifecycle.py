#!/usr/bin/env python3
"""F15-05 on Unix: bash, zsh and fish completion scripts install, and completion offers commands and machine names."""
import argparse
import json
from pathlib import Path
import shutil
import subprocess
import tempfile

PB = Path.home() / '.local/bin/pb'


def run(*args, timeout=60):
    result = subprocess.run([str(PB), *args], capture_output=True, text=True, timeout=timeout,
                            stdin=subprocess.DEVNULL)
    return result.returncode, result.stdout


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('machine')
    args = parser.parse_args()
    scripts = {}
    for shell in ('bash', 'zsh', 'fish', 'powershell'):
        code, text = run('completion', shell)
        if code or len(text) < 500 or 'pb' not in text:
            raise RuntimeError(shell + ' completion script was not generated')
        scripts[shell] = text
    with tempfile.TemporaryDirectory(prefix='paperboat-completion-') as directory:
        bash = Path(directory) / 'pb.bash'
        bash.write_text(scripts['bash'])
        # Sourcing registers a completion function for pb in a real bash.
        registered = subprocess.run(['bash', '-c', 'source "$1"; complete -p pb', 'x', str(bash)],
                                    capture_output=True, text=True, timeout=30)
        if registered.returncode or ' pb' not in registered.stdout:
            raise RuntimeError('bash did not register the pb completion')
        if shutil.which('fish'):
            fish = Path(directory) / 'pb.fish'
            fish.write_text(scripts['fish'])
            if subprocess.run(['fish', '-c', 'source ' + str(fish)], capture_output=True, timeout=30).returncode:
                raise RuntimeError('fish rejected the completion script')
        if shutil.which('zsh'):
            zsh = Path(directory) / '_pb'
            zsh.write_text(scripts['zsh'])
            if subprocess.run(['zsh', '-n', str(zsh)], capture_output=True, timeout=30).returncode:
                raise RuntimeError('zsh rejected the completion script')
    commands = run('__complete', '')[1]
    if not all(name in commands for name in ('exec', 'doctor', 'machine')):
        raise RuntimeError('root completion does not offer the main commands')
    missing = [name for name in ('exec', 'connect', 'ssh', 'ping', 'machine revoke')
               if args.machine not in run('__complete', *name.split(), '')[1]]
    if missing:
        raise RuntimeError('machine names are not completed for: ' + ', '.join(missing))
    print(json.dumps({'check': 'completion-lifecycle', 'status': 'PASS', 'machine': args.machine,
                      'scripts': sorted(scripts), 'bash_registered': True, 'commands_complete': True,
                      'machine_names_complete': True}), flush=True)


if __name__ == '__main__':
    main()
