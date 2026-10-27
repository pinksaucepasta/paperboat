#!/usr/bin/env python3
"""Final Linux re-check: every Linux feature (F) and command (C) row of CHECKLIST.md on one build.

  final_recheck.py inventory [--out FILE]      row -> proving checks, from each row's Linux evidence; flags [x] rows with none
  final_recheck.py run --build dev-X [options] run the checks against one build and write one result table

Check kinds (nothing new: each runs through an existing tool)
  smoke    ids of smoke.sh (green/hp.list and test_application_pairs); run once through smoke.sh --only-list on hp containers
  go       named Go tests found in the evidence; run through tools/ci-test.sh <module> -run (GitHub Actions, committed HEAD)
  console  console/scripts/check-*.js on the live console, through console/scripts/run-console-check.mjs
  device   a device script run inside container A of --ct-map (needs a peer: container B)
  cmd      a device script that needs arguments, declared in final_recheck.tsv with its command line
The extras of the final check (official Latest -> build update with history, published one-liner install, exec right
after a fresh pair) are cmd entries with rows EXTRA. Results: results/final-recheck-<build>-<UTC>.{md,json}.

Options of run: --binary (the final linux build file, for the extras), --release, --ct-map 'alias1=hp:ct1,alias2=hp:ct2' (the machine alias of each container, which is its smoke device name), --origin/--state (live console and
the signed-in storage state of its owner), --only ROW,ROW|CHECK,CHECK (a subset, for a dry run), --dry (print, run nothing).
Secrets come from the environment (HP_SUDO_PASS ...), never from arguments, and are never printed.
"""
import argparse
import collections
import datetime
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import time

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
MODULES = ('cli', 'server', 'tunnel', 'relay', 'shared', 'helper')
BAD = ('[!]', '[ ]')


def rows():
    """(id, linux state, evidence) of every F and C row; command rows carry the Linux cell one column later."""
    found = []
    for line in (ROOT / 'CHECKLIST.md').read_text().splitlines():
        cells = [c.strip() for c in line.strip().strip('|').split(' | ')]
        if not re.fullmatch(r'F\d+-\d+|C\d+', cells[0]):
            continue
        at = 3 if cells[0].startswith('C') else 2
        found.append((cells[0], cells[at][:3], ' | '.join(cells[at + 3:])))
    return found


def smoke_ids():
    text = (HERE / 'smoke.sh').read_text()
    ids = set(re.findall(r'^\s*test_application_pairs\s+(\S+)', text, re.M)) | set(re.findall(r'skipped\s+"?([a-z]+(?:-[a-z]+)+)"?\s', text))
    for listing in (HERE / 'green').glob('*.list'):
        for line in listing.read_text().splitlines():
            if line.strip() and not line.startswith('#'):
                ids.add(line.strip().partition(':')[0])
    return {i for i in ids if re.fullmatch(r'[a-z]+(?:-[a-z]+)*', i)} - {'version', 'doctor'}


def script_groups():
    """script -> smoke groups that run it (test_application_pairs kind script): a row naming the script is proved by those groups."""
    found = collections.defaultdict(set)
    for kind, script in re.findall(r'^\s*test_application_pairs\s+(\S+)\s+(\S+)', (HERE / 'smoke.sh').read_text(), re.M):
        found[script].add(kind)
    return found


def go_tests():
    """test name -> module, from the declarations (one git grep)."""
    out = subprocess.run(['git', 'grep', '-hnE', r'^func (Test\w+)\(', '--', '*_test.go'], cwd=ROOT, capture_output=True, text=True)
    where = {}
    for path_line in subprocess.run(['git', 'grep', '-nE', r'^func (Test\w+)\(', '--', '*_test.go'], cwd=ROOT, capture_output=True, text=True).stdout.splitlines():
        path, _, rest = path_line.partition(':')
        name = re.search(r'func (Test\w+)\(', rest)
        if name and path.split('/')[0] in MODULES:
            where.setdefault(name.group(1), path.split('/')[0])
    return where


def manifest():
    """final_recheck.tsv: check, kind, rows, spec. A rows entry @name.py stands for every row whose Linux evidence names that script."""
    named = collections.defaultdict(set)
    for row, _, evidence in rows():
        for script in re.findall(r'\b[A-Za-z0-9_-]+\.(?:py|sh|mjs|js)\b', evidence):
            named[script].add(row)
    entries = []
    for line in (HERE / 'final_recheck.tsv').read_text().splitlines():
        if line.strip() and not line.startswith('#'):
            check, kind, covers, spec = line.split('\t', 3)
            covered = set()
            for item in filter(None, covers.split(',')):
                covered |= named[item[1:]] if item.startswith('@') else {item}
            entries.append({'check': check, 'kind': kind, 'rows': sorted(covered), 'spec': spec})
    return entries


def inventory():
    ids, tests, entries = smoke_ids(), go_tests(), manifest()
    declared = collections.defaultdict(list)
    for entry in entries:
        for row in entry['rows']:
            declared[row].append(entry)
    groups = script_groups()
    scripts = {p.name for p in HERE.glob('*') if p.suffix in ('.py', '.sh', '.mjs')}
    consoles = {p.name for p in (ROOT / 'console/scripts').glob('check-*')}
    table = []
    for row, state, evidence in rows():
        # an id counts when the evidence names it as smoke:<id> or with its device pair (<id>:a-a), never as a plain word
        named = re.findall(r'smoke:([a-z]+(?:-[a-z]+)*)|\b([a-z]+(?:-[a-z]+)*):[a-z0-9]+-[a-z0-9]+\b', evidence)
        smoke = sorted({m for pair in named for m in pair if m in ids})
        go = sorted({t for t in re.findall(r'\bTest[A-Z]\w+', evidence) if t in tests})
        refs = sorted({m for m in re.findall(r'\b[A-Za-z0-9_-]+\.(?:py|sh|mjs|js)\b', evidence) if m in scripts | consoles})
        smoke = sorted(set(smoke) | {g for script in refs for g in groups.get(script, ()) if g in ids})
        mine = [e['check'] for e in declared.get(row, [])]
        table.append({'row': row, 'state': state, 'smoke': smoke, 'go': go, 'scripts': refs, 'declared': mine,
                      'runnable': bool(smoke or go or mine)})
    return table


def cmd_inventory(args):
    table = inventory()
    lines = ['row\tlinux\tsmoke\tgo\tscripts\tdeclared\trunnable']
    for r in table:
        lines.append('\t'.join([r['row'], r['state'], ','.join(r['smoke']), ','.join(r['go']), ','.join(r['scripts']),
                                ','.join(r['declared']), 'yes' if r['runnable'] else 'NO'] ))
    if args.out:
        Path(args.out).write_text('\n'.join(lines) + '\n')
    done = [r for r in table if r['state'] == '[x]']
    flagged = [r for r in done if not r['runnable']]
    kinds = collections.Counter()
    for r in done:
        kinds['smoke'] += bool(r['smoke']); kinds['go'] += bool(r['go']); kinds['declared'] += bool(r['declared'])
    print('%d F/C rows, %d Linux [x]; runnable through smoke/go/manifest: %d (smoke %d, go %d, manifest %d)' % (
        len(table), len(done), len(done) - len(flagged), kinds['smoke'], kinds['go'], kinds['declared']))
    print('[x] rows with no runnable check (%d):' % len(flagged))
    print(' '.join(r['row'] for r in flagged))
    print('rows not [x] on Linux: %s' % ' '.join('%s%s' % (r['row'], r['state']) for r in table if r['state'] != '[x]'))
    print('(scripts named in the evidence but not declared in final_recheck.tsv are listed per row in the TSV)')


def run(argv, env=None, stdin=None, timeout=3600):
    t0 = time.time()
    result = subprocess.run(argv, cwd=ROOT, capture_output=True, text=True, env={**os.environ, **(env or {})}, input=stdin, timeout=timeout)
    return result.returncode, result.stdout + result.stderr, int(time.time() - t0)


def pair_ids(groups, labels):
    """Exact smoke ids of the given groups for these device labels, taken from the green lists (hp, and hp+dadape for pairs)."""
    names = {'hp': labels[0], 'dadape': labels[-1]}
    found = set()
    for listing in (HERE / 'green').glob('*.list'):
        for line in listing.read_text().splitlines():
            line = line.strip()
            kind, _, pair = line.partition(':')
            if kind not in groups or line.startswith('#'):
                continue
            devices = pair.split('-') if pair else []
            if devices and all(d in names for d in devices):
                found.add('%s:%s' % (kind, '-'.join(names[d] for d in devices)))
    return found


def run_smoke(groups, args, results):
    labels = [pair.split('=')[0] for pair in args.ct_map.split(',')]
    ids = pair_ids(groups, labels)
    for group in sorted(set(groups) - {i.partition(':')[0] for i in ids}):
        known = bool(pair_ids({group}, ['hp', 'dadape']))
        results.append({'check': 'smoke:' + group, 'kind': 'smoke', 'status': 'SKIP' if known else 'FAIL', 'seconds': 0,
                        'detail': 'needs a second container (source and target differ)' if known else 'no id in green/*.list; add one so the final run knows how to run it'})
    if not ids:
        return
    listing = Path('/tmp/final-recheck-smoke.list')
    listing.write_text('\n'.join(sorted(ids)) + '\n')
    env = {'SMOKE_CT_MAP': args.ct_map, 'SMOKE_CT_DOCKER': os.environ.get('SMOKE_CT_DOCKER', 'docker'), 'SMOKE_CT_USER': 'pbt', 'SMOKE_CT_UID': '1000'}
    long_exec = ['--long-seconds', '1801'] if any(i.startswith('exec-long:') for i in ids) else []  # F4-02: a command of 30+ minutes
    code, out, secs = run([str(HERE / 'smoke.sh'), '--targets', ','.join(labels), '--expect-version', args.build, '--only-list', str(listing)] + long_exec, env)
    seen = {}
    for line in out.splitlines():
        m = re.match(r'^(\S+:\S+)\s+(PASS|FAIL|SKIP)\s+(\d+)s\s*(.*)$', line)
        if m:
            seen[m.group(1)] = (m.group(2), int(m.group(3)), m.group(4))
    for group in sorted({i.partition(':')[0] for i in ids}):
        mine = {k: v for k, v in seen.items() if k.partition(':')[0] == group}
        status = 'FAIL' if not mine or any(v[0] == 'FAIL' for v in mine.values()) else 'PASS' if any(v[0] == 'PASS' for v in mine.values()) else 'SKIP'
        detail = '; '.join('%s %s %s' % (k, v[0], v[2][:80]) for k, v in sorted(mine.items()) if v[0] != 'PASS')[:300] or ('%d runs' % len(mine))
        results.append({'check': 'smoke:' + group, 'kind': 'smoke', 'status': status, 'seconds': sum(v[1] for v in mine.values()),
                        'detail': detail if mine else 'smoke.sh printed no result (exit %d)' % code})


def run_go(tests, results):
    by_module = collections.defaultdict(list)
    where = go_tests()
    for name in tests:
        by_module[where[name]].append(name)
    for module, names in sorted(by_module.items()):
        code, out, secs = run([str(ROOT / 'tools/ci-test.sh'), module, '-run', '^(%s)$' % '|'.join(sorted(names))])
        failed = set(re.findall(r'--- FAIL: (Test\w+)', out))
        url = (re.findall(r'https://github.com/\S+/actions/runs/\d+', out) or [''])[-1]
        for name in names:
            ok = code == 0 and name not in failed
            results.append({'check': 'go:%s/%s' % (module, name), 'kind': 'go', 'status': 'PASS' if ok else 'FAIL', 'seconds': secs,
                            'detail': url if ok else (url + ' ' + ('failed' if name in failed else 'module run failed')).strip()})


DOCKER = os.environ.get('SMOKE_CT_DOCKER', 'docker')


def in_container(host, ct, command, stdin=None, timeout=3600):
    remote = '%s exec -i -u pbt -w /home/pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 %s bash -lc %s' % (DOCKER, ct, shlex.quote(command))
    return run(['ssh', '-o', 'BatchMode=yes', host, remote], stdin=stdin, timeout=timeout)


def stage_scripts(host, ct):
    """The device scripts run inside the container from /home/pbt/ds (the same files smoke.sh uploads)."""
    tar = subprocess.run(['tar', 'czf', '-', '-C', str(HERE)] + sorted(p.name for p in HERE.iterdir() if p.suffix in ('.py', '.sh')),
                         capture_output=True, check=True).stdout
    remote = '%s exec -i -u pbt %s bash -c %s' % (DOCKER, ct, shlex.quote('rm -rf ~/ds && mkdir -p ~/ds && tar xzf - -C ~/ds'))
    subprocess.run(['ssh', host, remote], input=tar, check=True, capture_output=True)


def machine_name(host, ct):
    code, out, _ = in_container(host, ct, "python3 - <<'EOF'\nimport json,subprocess\nme=json.load(open('.local/state/paperboat/runtime/machine-registration.json'))['machine_id']\n"
                                "for m in json.loads(subprocess.run(['.local/bin/pb','machine','list','--json'],capture_output=True,text=True).stdout)['machines']:\n"
                                "    print(m['alias']) if m['id']==me else None\nEOF")
    return out.strip().splitlines()[-1] if code == 0 and out.strip() else ''


def containers(ct_map):
    return [(pair.partition('=')[0], pair.partition('=')[2].partition(':')[0], pair.partition('=')[2].partition(':')[2]) for pair in ct_map.split(',') if pair]


def run_entry(entry, args, results):
    """console, device and cmd entries. Placeholders: {build} {binary} {origin} {state} {host} {self} {peer} (machine names) {ct_a} {ct_b} (containers)."""
    devices = containers(args.ct_map)
    names = [machine_name(host, ct) for _, host, ct in devices]
    values = {'build': args.build, 'origin': args.origin, 'state': args.state, 'host': args.host, 'binary': args.binary, 'release': args.release,
              'self': names[0] if names else '', 'peer': names[-1] if names else '',
              'ct_a': devices[0][2] if devices else '', 'ct_b': devices[-1][2] if devices else ''}
    spec = entry['spec'].format(**values)
    env = {'ORIGIN': args.origin, 'STATE': args.state, 'HOST': args.host, 'BINARY': args.binary, 'BUILD': args.build}  # PW_CHROMIUM and PW_MODULE come from the caller
    if entry['kind'] == 'device':
        stage_scripts(devices[0][1], devices[0][2])
        code, out, secs = in_container(devices[0][1], devices[0][2], 'cd ~/ds && ' + spec, stdin=os.environ.get('HP_SUDO_PASS', '') + '\n')
    elif entry['kind'] == 'console':
        script, _, rest = spec.partition(' ')
        code, out, secs = run(['node', str(ROOT / 'console/scripts/run-console-check.mjs'), str(ROOT / 'console/scripts' / script)] + shlex.split(rest), env)
    else:
        code, out, secs = run(['bash', '-c', spec], env)
    lines = [l for l in out.strip().splitlines() if l.strip()]
    shown = [l for l in lines if 'FAIL' in l][:2] or lines[-1:] or ['']
    results.append({'check': entry['check'], 'kind': entry['kind'], 'status': 'PASS' if code == 0 else 'FAIL', 'seconds': secs, 'detail': ' / '.join(shown)[:300]})


def cmd_run(args):
    table = inventory()
    entries = manifest()
    wanted = set(filter(None, args.only.split(',')))
    chosen = [r for r in table if not wanted or r['row'] in wanted]
    ids = {i for r in chosen for i in r['smoke']}
    tests = {t for r in chosen for t in r['go']}
    todo = [e for e in entries if not wanted or e['check'] in wanted or set(e['rows']) & wanted or ('EXTRA' in e['rows'] and 'EXTRA' in wanted)]
    if wanted:
        ids = {i for i in ids if i in wanted or any(i in r['smoke'] and r['row'] in wanted for r in chosen)}
    if args.dry:
        print('smoke ids:', sorted(ids)); print('go tests:', len(tests)); print('entries:', [e['check'] for e in todo])
        return 0
    results = []
    if ids:
        run_smoke(ids, args, results)
    if tests:
        run_go(sorted(tests), results)
    for entry in todo:
        run_entry(entry, args, results)
    by_check = {r['check']: r for r in results}
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    out_dir = HERE / 'results'
    out_dir.mkdir(exist_ok=True)
    per_row = []
    for r in chosen:
        mine = ['smoke:' + i for i in r['smoke']] + ['go:%s/%s' % (go_tests()[t], t) for t in r['go']] + r['declared']
        got = [by_check[c]['status'] for c in mine if c in by_check]
        status = 'FAIL' if 'FAIL' in got else 'PASS' if got else ('NO CHECK' if r['state'] == '[x]' else r['state'])
        per_row.append((r['row'], r['state'], status, len(got)))
    md = ['# Final Linux re-check %s (%s)' % (args.build, stamp), '', '| check | kind | status | s | detail |', '|---|---|---|---|---|']
    md += ['| %s | %s | %s | %d | %s |' % (r['check'], r['kind'], r['status'], r['seconds'], r['detail'].replace('|', '/')) for r in results]
    md += ['', '| row | Linux | result | checks run |', '|---|---|---|---|'] + ['| %s | %s | %s | %d |' % x for x in per_row]
    (out_dir / ('final-recheck-%s-%s.md' % (args.build, stamp))).write_text('\n'.join(md) + '\n')
    (out_dir / ('final-recheck-%s-%s.json' % (args.build, stamp))).write_text(json.dumps({'build': args.build, 'checks': results, 'rows': per_row}, indent=1))
    print('\n'.join(md))
    return 1 if any(r['status'] == 'FAIL' for r in results) else 0


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest='command', required=True)
    inv = sub.add_parser('inventory')
    inv.add_argument('--out')
    go = sub.add_parser('run')
    go.add_argument('--build', required=True)
    go.add_argument('--ct-map', default='')
    go.add_argument('--host', default='hp')
    go.add_argument('--release', default='', help='version of the final official release (for the published-install extra)')
    go.add_argument('--binary', default='', help='path of the final linux build (for the update and fresh-pair extras)')
    go.add_argument('--origin', default=os.environ.get('PB_CONSOLE_ORIGIN', ''))
    go.add_argument('--state', default='')
    go.add_argument('--only', default='')
    go.add_argument('--dry', action='store_true')
    args = parser.parse_args()
    if args.command == 'inventory':
        return cmd_inventory(args)
    return cmd_run(args)


if __name__ == '__main__':
    sys.exit(main() or 0)
