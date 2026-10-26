#!/usr/bin/env python3
"""C089: bare tunnel/help succeeds; bare JSON is refused clearly without side effects."""
import json
from pathlib import Path

from pairing_lifecycle import command_result


def main():
    pb = Path.home() / '.local/bin/pb'
    cases = []
    for name, words, success in [('bare', [], True), ('help', ['--help'], True),
                                 ('error', ['--json'], False)]:
        argv = ['tunnel', *words]
        result = command_result(pb, *argv, input='', timeout=30)
        text = result.stdout + result.stderr
        clean = bool(text.strip()) and not any(x in text for x in
                    ('panic:', 'goroutine ', 'Traceback', 'nil pointer'))
        if success:
            valid = result.returncode == 0 and all(x in result.stdout for x in
                                                  ('Usage:', 'tunnel', 'create', 'list'))
        else:
            try:
                receipt = json.loads(result.stdout)
                valid = result.returncode != 0 and receipt['ok'] is False and bool(receipt['error']['message'])
            except (ValueError, KeyError, TypeError):
                valid = False
        case = {'case': name, 'argv': [str(pb), *argv], 'exit': result.returncode,
                'status': 'PASS' if valid and clean else 'FAIL'}
        if case['status'] == 'FAIL':
            case.update(stdout=result.stdout, stderr=result.stderr)
        cases.append(case)
    passed = all(x['status'] == 'PASS' for x in cases)
    print(json.dumps({'check': 'tunnel-entry', 'row': 'C089',
                      'status': 'PASS' if passed else 'FAIL', 'cases': cases}))
    raise SystemExit(0 if passed else 1)


if __name__ == '__main__':
    main()
