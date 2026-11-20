#!/usr/bin/env python3
"""R-01: check changed Go packages.
  pr-checks.py BASE          gofmt of every module, then vet, -race test, source policy and static analysis (staticcheck + deadcode) of each module with changed packages or reverse dependencies (merge queue groups)
  pr-checks.py BASE --fast   gofmt of every module, then go vet and source policy of each module with changed packages only (pull requests: seconds, no tests)"""
import json
import os
import subprocess
import sys
from pathlib import Path

MODULES = ('shared', 'server', 'cli', 'relay', 'edge', 'helper')
ROOT = Path(__file__).resolve().parents[1]
DATABASE_MODULES = ('server', 'edge', 'helper')


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


def test_packages(module, names):
    """-race tests. A database module's packages each get a fresh database from tools/pg-test-exec.sh (shared with
    server-ci.yml, test.yml and tools/remote-test.sh): packages sharing one database truncate each other's rows."""
    command = ['go', 'test', '-race', '-count=1']
    if os.environ.get('PAPERBOAT_TEST_DATABASE_DSN') and module in DATABASE_MODULES:
        command += ['-exec', str(ROOT / 'tools/pg-test-exec.sh')]
    subprocess.run(command + names, cwd=ROOT / module, check=True, timeout=1200)


def output(*argv, cwd=ROOT):
    return subprocess.check_output(argv, cwd=cwd, text=True)


def unformatted(root=ROOT):
    """Go files of every module that gofmt would rewrite: the whole tree, not the diff (seconds, no compile)."""
    return output('gofmt', '-l', *MODULES, cwd=root).split()


def main():
    fast = '--fast' in sys.argv[2:]
    paths = output('git', 'diff', '--name-only', sys.argv[1], 'HEAD').splitlines()
    if bad := unformatted():
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
            subprocess.run([ROOT / 'tools/verify-source-policy.sh', module], check=True, timeout=300)
            if not fast:
                test_packages(module, names)
                # Whole module: removing a shared function can leave code in a dependent module unused.
                subprocess.run([ROOT / 'tools/verify-static-analysis.sh', module], check=True, timeout=2400)
    print(f"{'Fast' if fast else 'Targeted'} checks passed ({len(chosen)} packages).")


if __name__ == '__main__':
    main()
