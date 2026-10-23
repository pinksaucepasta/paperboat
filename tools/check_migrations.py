#!/usr/bin/env python3
"""Expand/contract lint for server migrations (F21-04).

During a rollout the previous server version keeps serving on the migrated schema, so a release
may only expand it. A contract step (DROP TABLE/COLUMN, RENAME, column type change, SET NOT NULL)
ships in a later release than the expand step and the code change that stopped using the old
shape: it must carry `-- contract-after: NNN`, where migration NNN is already in the deployed
server (the newest origin/deployed/* branch). CREATE INDEX on a table that can be large must be
CONCURRENTLY (with `-- +goose NO TRANSACTION`); a small table says `-- small-table`.
Only migrations new relative to origin/main are checked, and only their Up section.
Prior art: GitLab migration_helpers / Rails strong_migrations (unsafe operations need an explicit
marker and a post-deploy step); Teleport lib/backend migrations run additive steps only.
"""
import re
import subprocess
import sys

MIGRATIONS = 'server/internal/db/migrations/'
CONTRACT = re.compile(r'\b(DROP\s+TABLE|DROP\s+COLUMN|RENAME\s+(COLUMN|TO)\b|ALTER\s+COLUMN\s+\S+\s+(SET\s+DATA\s+)?TYPE\b|SET\s+NOT\s+NULL)', re.I)
INDEX = re.compile(r'\bCREATE\s+(UNIQUE\s+)?INDEX\b(?!\s+CONCURRENTLY)', re.I)
MARK = re.compile(r'^--\s*contract-after:\s*(\d+)\s*$', re.M)


def git(*args):
    return subprocess.run(['git', *args], capture_output=True, text=True, check=True).stdout


def up_section(text):
    text = text.split('-- +goose Down')[0]
    return '\n'.join(line for line in text.splitlines() if not line.lstrip().startswith('--'))


def deployed_versions():
    branches = [b.strip() for b in git('branch', '-r', '--list', 'origin/deployed/*').splitlines() if b.strip()]
    if not branches:
        return None
    newest = max(branches, key=lambda b: [int(p) for p in re.findall(r'\d+', b.split('/', 2)[-1])])
    names = git('ls-tree', '--name-only', newest, MIGRATIONS).splitlines()
    return {int(m.group(1)) for n in names if (m := re.match(r'.*/(\d+)_', n))}


def check(name, text, deployed):
    errors = []
    up = up_section(text)
    if CONTRACT.search(up):
        mark = MARK.search(text)
        if not mark:
            errors.append(f'{name}: contract step (drop/rename/type/NOT NULL) needs "-- contract-after: NNN" naming '
                          'a deployed migration whose release already stopped using the old shape')
        elif deployed is not None and int(mark.group(1)) not in deployed:
            errors.append(f'{name}: contract-after {mark.group(1)} is not in the deployed server yet; '
                          'ship the expand step and its code first')
    if INDEX.search(up) and not re.search(r'^--\s*small-table\b', text, re.M):
        errors.append(f'{name}: CREATE INDEX without CONCURRENTLY locks writes on a large table; use CONCURRENTLY '
                      '(-- +goose NO TRANSACTION) or mark "-- small-table"')
    return errors


def main():
    base = git('merge-base', 'HEAD', 'origin/main').strip()
    added = [n for n in git('diff', '--name-only', '--diff-filter=A', base, '--', MIGRATIONS).splitlines()]
    added += [n for n in git('diff', '--name-only', '--diff-filter=A', '--', MIGRATIONS).splitlines()]
    deployed = deployed_versions()
    errors = []
    for name in sorted(set(added)):
        with open(name) as f:
            errors += check(name, f.read(), deployed)
    for e in errors:
        print('FAIL:', e)
    return 1 if errors else 0


if __name__ == '__main__':
    sys.exit(main())
