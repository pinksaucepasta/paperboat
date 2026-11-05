#!/usr/bin/env python3
"""Read `go test -json` on stdin, save every event to --save, and print what plain `go test` would print.

Prints each failing test's output and the package result lines; with --verbose (a narrowed -run) it prints
everything, as `go test -v` does, so `--- PASS: TestName` lines stay in the CI log. The saved events feed
tools/skip-audit.py. Exit status is go test's: use `set -o pipefail`.
"""
import argparse
import json
import sys


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--save', required=True)
    parser.add_argument('--verbose', action='store_true')
    args = parser.parse_args()
    buffers = {}
    with open(args.save, 'a') as saved:
        for line in sys.stdin:
            saved.write(line)
            try:
                event = json.loads(line)
            except ValueError:
                sys.stdout.write(line)  # build errors and other non-JSON output pass through
                continue
            output, test = event.get('Output', ''), event.get('Test')
            if not test:
                sys.stdout.write(output)  # package lines: ok, FAIL, panics outside a test
            elif args.verbose:
                sys.stdout.write(output)
            else:
                key = (event.get('Package'), test)
                buffers.setdefault(key, []).append(output)
                if event.get('Action') == 'fail':
                    sys.stdout.write(''.join(buffers.pop(key)))
                elif event.get('Action') in ('pass', 'skip'):
                    buffers.pop(key, None)
            sys.stdout.flush()


if __name__ == '__main__':
    main()
