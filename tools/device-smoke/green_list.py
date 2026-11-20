#!/usr/bin/env python3
"""Keep the per-device list of checks that have ever passed (the regression gate).

  green_list.py refresh TARGET   add every runnable check that ever passed on TARGET to green/TARGET.list
  green_list.py show TARGET      print green/TARGET.list
The list only grows: a check leaves it by being deleted from smoke.sh, never by failing.
"""
import json
from pathlib import Path
import re
import sys

HERE = Path(__file__).resolve().parent
from dev_targets import configured_path, device_names, load
BASELINE = {'version', 'doctor'}


def listing(target):
    return configured_path(load(), 'green_dir', HERE / 'green') / (target + '.list')


def current(target):
    path = listing(target)
    return [line.strip() for line in path.read_text().splitlines() if line.strip() and not line.startswith('#')] if path.exists() else []


def involves_only(check, target):
    suffix = check.partition(':')[2]
    names = '|'.join(re.escape(name) for name in sorted(device_names(), key=len, reverse=True))
    members = re.findall(r'(?<![a-z0-9])(' + names + r')(?![a-z0-9])', suffix)
    return set(members) == {target}


def has_runner(check, script):
    return re.search(r'(?<![\w-])' + re.escape(check.partition(':')[0]) + r'(?![\w-])', script) is not None


def ever_passed(results, script, target):
    found = set()
    for path in sorted(results.glob('*.json')):
        try:
            rows = json.loads(path.read_text()).get('results', [])
        except (ValueError, OSError):
            continue
        for row in rows:
            check = row.get('id', '')
            if row.get('status') == 'PASS' and ':' in check and check.partition(':')[0] not in BASELINE \
                    and involves_only(check, target) and has_runner(check, script):
                found.add(check)
    return found


def refresh(target):
    script = (HERE / 'smoke.sh').read_text()
    merged = sorted(set(current(target)) | ever_passed(configured_path(load(), 'results_dir', HERE / 'results'), script, target))
    listing(target).parent.mkdir(exist_ok=True)
    listing(target).write_text('# Checks that have ever passed on %s; every one must pass on a build before it becomes the base.\n' % target
                               + ''.join(check + '\n' for check in merged))
    return merged


if __name__ == '__main__':
    if len(sys.argv) != 3 or sys.argv[1] not in ('refresh', 'show'):
        sys.exit(__doc__)
    print('\n'.join(refresh(sys.argv[2]) if sys.argv[1] == 'refresh' else current(sys.argv[2])))
