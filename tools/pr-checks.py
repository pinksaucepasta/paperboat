#!/usr/bin/env python3
"""R-01: check changed Go packages.
  pr-checks.py BASE          vet and -race test the changed packages and their workspace reverse dependencies (merge queue groups)
  pr-checks.py BASE --fast   gofmt of the changed Go files and go vet of the changed packages only (pull requests: seconds, no tests)"""
import json
import os
import subprocess
import sys
from pathlib import Path
from urllib.parse import urlsplit, urlunsplit

MODULES = ('shared', 'server', 'cli', 'relay', 'tunnel', 'helper', 'sdk/go')
ROOT = Path(__file__).resolve().parents[1]
DATABASE_MODULES = ('server', 'tunnel', 'helper')


def affected(packages, paths, transitive=True):
    selected = set()
    for name, p in packages.items():
        directory = p['directory']
        module = p['module']
        if any(path in ('go.work', 'go.work.sum') or
               path in (module + '/go.mod', module + '/go.sum') or
               path.startswith(module + '/internal/db/migrations/') or
               path.startswith(directory + '/') and not any(
                   other['directory'].startswith(directory + '/') and
                   path.startswith(other['directory'] + '/') for other in packages.values())
               for path in paths):
            selected.add(name)
    # Deleted/moved packages still have importers; include their old import path.
    for path in paths:
        for name, p in packages.items():
            prefix = p['module'] + '/'
            if path.startswith(prefix):
                old = p['module_path'] + '/' + str(Path(path).parent.relative_to(p['module']))
                if old not in packages:
                    selected.add(old)
                break
    while transitive:
        expanded = selected | {name for name, p in packages.items() if selected.intersection(p['imports'])}
        if expanded == selected:
            return sorted(selected.intersection(packages))
        selected = expanded
    return sorted(selected.intersection(packages))


def database_dsn(dsn, name):
    """The same server with another database name."""
    parts = urlsplit(dsn)
    return urlunsplit(parts._replace(path='/' + name))


def database_name(index):
    """The server's test guards refuse any database whose name does not end in _test (auth_integration_test.go, abusereports)."""
    return f'paperboat_pr_{index}_test'


def test_packages(module, names):
    """-race tests. Every package of a database module gets its own fresh database, as the lane-test workflow and
    tools/remote-test.sh do: packages sharing one database truncate each other's rows (deadlocks, foreign key errors)."""
    command = ['go', 'test', '-race', '-count=1']
    dsn = os.environ.get('PAPERBOAT_TEST_DATABASE_DSN')
    if not dsn or module not in DATABASE_MODULES:
        subprocess.run(command + names, cwd=ROOT / module, check=True, timeout=1200)
        return
    for index, name in enumerate(names):
        database = database_name(index)
        subprocess.run(['psql', database_dsn(dsn, 'postgres').split('?')[0], '-qv', 'ON_ERROR_STOP=1',
                        '-c', f'drop database if exists "{database}" with (force)', '-c', f'create database "{database}"'], check=True, timeout=120)
        subprocess.run(command + [name], cwd=ROOT / module, check=True, timeout=1200,
                       env=dict(os.environ, PAPERBOAT_TEST_DATABASE_DSN=database_dsn(dsn, database)))


def output(*argv, cwd=ROOT):
    return subprocess.check_output(argv, cwd=cwd, text=True)


def unformatted(paths):
    """Changed Go files that gofmt would rewrite (deleted files are skipped)."""
    files = [str(ROOT / p) for p in paths if p.endswith('.go') and (ROOT / p).is_file()]
    return output('gofmt', '-l', *files).split() if files else []


def main():
    fast = '--fast' in sys.argv[2:]
    paths = output('git', 'diff', '--name-only', sys.argv[1], 'HEAD').splitlines()
    if fast and (bad := unformatted(paths)):
        sys.exit('gofmt would change: ' + ' '.join(bad) + ' (run gofmt -w on them)')
    packages = {}
    for module in MODULES:
        text = output('go', 'list', '-json', './...', cwd=ROOT / module)
        decoder = json.JSONDecoder()
        while text.strip():
            p, end = decoder.raw_decode(text.lstrip())
            text = text.lstrip()[end:]
            packages[p['ImportPath']] = {
                'directory': str(Path(p['Dir']).relative_to(ROOT)), 'module': module,
                'module_path': p['Module']['Path'],
                'imports': p.get('Imports', []) + p.get('TestImports', []) + p.get('XTestImports', [])}
    chosen = affected(packages, paths, transitive=not fast)
    for module in MODULES:
        names = [name for name in chosen if packages[name]['module'] == module]
        if names:
            print(f'{module}: ' + ' '.join(names), flush=True)
            subprocess.run(['go', 'vet'] + names, cwd=ROOT / module, check=True, timeout=1200)
            if not fast:
                test_packages(module, names)
    print(f"{'Fast' if fast else 'Targeted'} checks passed ({len(chosen)} packages).")


if __name__ == '__main__':
    main()
