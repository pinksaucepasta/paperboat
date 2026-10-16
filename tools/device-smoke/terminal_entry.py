#!/usr/bin/env python3
"""Exercise the normal root and session menus without acting on other sessions."""
import argparse
import json
from pathlib import Path
import sys

from pairing_lifecycle import command_diagnostic
from terminal_lifecycle import Terminal, qualify


def menu(binary, session_menu):
    terminal = Terminal([binary, 'session'] if session_menu else [binary])
    try:
        if session_menu:
            terminal.expect('Machine sessions')
            terminal.send('\r')
            terminal.expect('All machine sessions, newest first')
            terminal.send(b'\x1b')
            terminal.expect('Machine sessions')
        else:
            terminal.expect('What do you want to do?')
            terminal.expect('Machines')
        terminal.send(b'\x1b')
        code = terminal.wait_exit()
        if code != 0:
            raise RuntimeError('normal menu exit returned a failure status')
    except Exception as error:
        diagnostic = command_diagnostic(terminal.arguments, terminal.output.decode(errors='replace'), '')
        raise RuntimeError('terminal entry failed; private diagnostics: ' + str(diagnostic)) from error
    finally:
        terminal.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    binary = Path.home() / '.local/bin/pb'
    menu(binary, False)
    menu(binary, True)
    qualify(binary, args.target, 'main', 'htop', entry='root')
    print(json.dumps({'check': 'terminal-entry', 'target': args.target, 'status': 'PASS',
                      'root_menu': True, 'session_catalog': True, 'root_attach': True}), flush=True)


if __name__ == '__main__':
    main()
