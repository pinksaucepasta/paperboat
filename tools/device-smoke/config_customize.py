#!/usr/bin/env python3
"""C229-C231: pb config customize (editor), explain and import, against an isolated temporary --config.

Runs on the machine under test: `python3 config_customize.py`. Nothing outside a temporary directory changes.
  C231 import: a shortcut + exec default document is validated and saved; show returns it; a bad file is refused
  C230 explain: the shortcut expands to exec ... uptime with executes=false and runs nothing
  C229 editor: Appearance -> Theme -> mono -> Save changes in a real pty; the saved file has theme mono and keeps
       the imported shortcut; reset (preview code, then --confirm) restores the defaults
"""
import json
import os
from pathlib import Path
import pty
import re
import select
import subprocess
import tempfile
import termios
import time

import winhost

PB = winhost.pb_path()  # pb.exe on the Windows device when PB_SMOKE_WINDOWS_PB is set; its files live there
SHORTCUT = 'smokeuptime'


def main():
    with winhost.workdir('pb-customize-') as directory:
        root = ['--config', str(directory / 'config.json')]

        def run(*args):
            r = winhost.pb_text(PB, [*root, *args], timeout=60, stdin=subprocess.DEVNULL)
            return r.returncode, r.stdout + r.stderr

        def data(*args):
            split = args.index('--') if '--' in args else len(args)  # after `--` --json would be an argument
            code, out = run(*args[:split], '--json', *args[split:])
            receipt = json.loads(out)
            assert code == 0 and receipt.get('ok') is True, ' '.join(args) + ': ' + out[-200:]
            return receipt['data']

        result = {'check': 'config-customize', 'status': 'PASS'}
        if winhost.device():
            path = winhost.RemotePath(winhost.device(), data('config', 'customize', 'path')['path'])
            assert path.parent == directory, path
        else:
            path = Path(data('config', 'customize', 'path')['path'])
            assert path.parent == Path(directory).resolve() or path.parent == Path(directory), path
        default = data('config', 'customize', 'show')['preferences']

        document = dict(default, shortcuts={SHORTCUT: {'command': ['exec'], 'args': ['{1}', '--', 'uptime']}},
                        defaults={'exec': {'timeout': '45s'}})
        candidate, bad = directory / 'in.json', directory / 'bad.json'
        candidate.write_text(json.dumps(document))
        bad.write_text(json.dumps(dict(document, shortcuts={'x': {'command': ['no-such-command']}})))
        assert run('config', 'customize', 'import', str(bad), '--json')[0] != 0, 'an invalid document was imported'
        data('config', 'customize', 'import', str(candidate))
        data('config', 'customize', 'validate')
        shown = data('config', 'customize', 'show')['preferences']
        assert SHORTCUT in shown['shortcuts'] and shown['defaults'] == {'exec': {'timeout': '45s'}}, shown
        result['C231_import_validate_show'] = True

        explained = data('config', 'customize', 'explain', '--', SHORTCUT, 'machine1')
        assert explained['executes'] is False and explained['shortcut'] == SHORTCUT, explained
        assert 'exec' in explained['argv'] and 'uptime' in explained['argv'] and 'machine1' in explained['argv'], explained
        result['C230_explain_no_execution'] = True

        editor(root)
        saved = data('config', 'customize', 'show')['preferences']
        assert saved['tui']['theme'] == 'mono' and SHORTCUT in saved['shortcuts'], saved['tui']
        result['C229_editor_saves_theme'] = True

        preview = run('config', 'customize', 'reset')[1]
        code = re.search(r'--confirm ([A-Z0-9]{6})', preview)
        assert code, 'reset printed no confirmation code'
        data('config', 'customize', 'reset', '--confirm', code.group(1))
        assert data('config', 'customize', 'show')['preferences'] == default, 'reset did not restore defaults'
        result['reset_restores_defaults'] = True
    print(json.dumps(result), flush=True)


def editor(root):
    """Drive the real editor: type to filter, Enter to pick, at each screen."""
    argv = winhost.terminal_argv(PB, [*root, 'config', 'customize'])  # on the device: its console through ssh -tt
    pid, fd = pty.fork()
    if pid == 0:
        os.execvpe(argv[0], argv, dict(os.environ, **winhost.terminal_env(PB)))
    termios.tcsetwinsize(fd, (40, 120))
    seen, deadline = b'', time.monotonic() + 60
    steps = [(b'Make Paperboat yours', 'Appearance'), (b'Accent color', 'Theme'), (b'Current selection', 'mono'),
             (b'Accent color', 'Done'), (b'Make Paperboat yours', 'Save changes')]
    try:
        for marker, keys in steps:
            while marker not in seen:
                assert time.monotonic() < deadline, 'editor never showed %r' % marker
                if select.select([fd], [], [], 1)[0]:
                    seen += os.read(fd, 65536)
            seen = b''
            time.sleep(0.3)
            os.write(fd, keys.encode())
            time.sleep(0.3)
            os.write(fd, b'\r')
        while time.monotonic() < deadline:
            done, status = os.waitpid(pid, os.WNOHANG)
            if done:
                assert os.waitstatus_to_exitcode(status) == 0, 'editor exited %d' % os.waitstatus_to_exitcode(status)
                return
            if select.select([fd], [], [], 1)[0]:
                try:
                    os.read(fd, 65536)
                except OSError:
                    pass
        raise AssertionError('editor did not exit after Save changes')
    finally:
        try:
            os.kill(pid, 9)
            os.waitpid(pid, 0)
        except (ProcessLookupError, ChildProcessError):
            pass
        os.close(fd)


if __name__ == '__main__':
    main()
