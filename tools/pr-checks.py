#!/usr/bin/env python3
"""R-01: vet/test changed Go packages and their workspace reverse dependencies."""
import json
import subprocess
import sys
from pathlib import Path

MODULES = ('shared', 'server', 'cli', 'relay', 'tunnel', 'helper', 'sdk/go')
ROOT = Path(__file__).resolve().parents[1]


def affected(packages, paths):
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
    while True:
        expanded = selected | {name for name, p in packages.items() if selected.intersection(p['imports'])}
        if expanded == selected:
            return sorted(selected.intersection(packages))
        selected = expanded


def output(*argv, cwd=ROOT):
    return subprocess.check_output(argv, cwd=cwd, text=True)


def main():
    paths = output('git', 'diff', '--name-only', sys.argv[1], 'HEAD').splitlines()
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
    chosen = affected(packages, paths)
    for module in MODULES:
        names = [name for name in chosen if packages[name]['module'] == module]
        if names:
            print(f'{module}: ' + ' '.join(names), flush=True)
            for command in (['go', 'vet'], ['go', 'test', '-race', '-count=1']):
                subprocess.run(command + names, cwd=ROOT / module, check=True, timeout=1200)
    print(f'Targeted checks passed ({len(chosen)} packages).')


if __name__ == '__main__':
    main()
