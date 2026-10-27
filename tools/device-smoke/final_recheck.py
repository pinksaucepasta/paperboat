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
The extras of the final check (previous official -> final update with history, exec right after a fresh pair, ...) are cmd
entries with rows EXTRA. Results: results/final-recheck-<build>-<UTC>.{md,json}.

Nothing is reused: `run` makes its own throwaway console account (disposable mail.tm inbox, deleted at the end), enrolls fresh
containers on --host with the published one-liner (the official Latest, which must be --build; "latest" takes whatever is
published) and tears all of it down again, so the check starts from what a new user has. Needs ORIGIN (--origin) and PW_CHROMIUM
(+ PW_MODULE) for the console.
Options of run: --build (the official release under test), --previous (the previous official release the update-with-history
extra starts from), --host (the Docker host), --origin (live console), --only ROW,ROW|CHECK,CHECK (a subset, for a dry run),
--dry (print, run nothing), --keep (leave the containers and the account for a look; the next run does not reuse them).
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
import urllib.request

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
    env = {'SMOKE_CT_MAP': args.ct_map, 'SMOKE_CT_DOCKER': os.environ.get('SMOKE_CT_DOCKER', 'docker'), 'SMOKE_CT_USER': CT_USER, 'SMOKE_CT_UID': CT_UID}
    long_exec = ['--long-seconds', '1801'] if any(i.startswith('exec-long:') for i in ids) else []  # F4-02: a command of 30+ minutes
    code, out, secs = run([str(HERE / 'smoke.sh'), '--targets', ','.join(labels), '--expect-version', args.version, '--only-list', str(listing)] + long_exec, env)
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
CT_USER = os.environ.get('SMOKE_CT_USER', 'pbt')  # the containers' user and uid, as for smoke.sh
CT_UID = os.environ.get('SMOKE_CT_UID', '1000')


def in_container(host, ct, command, stdin=None, timeout=3600):
    remote = '%s exec -i -u %s -w /home/%s -e HOME=/home/%s -e XDG_RUNTIME_DIR=/run/user/%s %s bash -lc %s' % (DOCKER, CT_USER, CT_USER, CT_USER, CT_UID, ct, shlex.quote(command))
    return run(['ssh', '-o', 'BatchMode=yes', host, remote], stdin=stdin, timeout=timeout)


def stage_scripts(host, ct):
    """The device scripts run inside the container from /home/<user>/ds (the same files smoke.sh uploads)."""
    tar = subprocess.run(['tar', 'czf', '-', '-C', str(HERE)] + sorted(p.name for p in HERE.iterdir() if p.suffix in ('.py', '.sh')),
                         capture_output=True, check=True).stdout
    remote = '%s exec -i -u %s -e HOME=/home/%s %s bash -c %s' % (DOCKER, CT_USER, CT_USER, ct, shlex.quote('rm -rf ~/ds && mkdir -p ~/ds && tar xzf - -C ~/ds'))
    subprocess.run(['ssh', host, remote], input=tar, check=True, capture_output=True)


def machine_name(host, ct):
    code, out, _ = in_container(host, ct, "python3 - <<'EOF'\nimport json,subprocess\nme=json.load(open('.local/state/paperboat/runtime/machine-registration.json'))['machine_id']\n"
                                "for m in json.loads(subprocess.run(['.local/bin/pb','machine','list','--json'],capture_output=True,text=True).stdout)['machines']:\n"
                                "    print(m['alias']) if m['id']==me else None\nEOF")
    return out.strip().splitlines()[-1] if code == 0 and out.strip() else ''


def containers(ct_map):
    return [(pair.partition('=')[0], pair.partition('=')[2].partition(':')[0], pair.partition('=')[2].partition(':')[2]) for pair in ct_map.split(',') if pair]


def run_entry(entry, args, results):
    """console, device and cmd entries. Placeholders: {build} {previous} {origin} {state} {host} {self} {peer} (machine names) {ct_a} {ct_b} (containers)."""
    devices = containers(args.ct_map)
    names = [machine_name(host, ct) for _, host, ct in devices]
    values = {'build': args.build, 'version': getattr(args, 'version', '') or args.build, 'origin': args.origin, 'state': args.state, 'host': args.host, 'previous': args.previous,
              'self': names[0] if names else '', 'peer': names[-1] if names else '',
              'ct_a': devices[0][2] if devices else '', 'ct_b': devices[-1][2] if devices else '', 'ct_user': CT_USER, 'ct_uid': CT_UID}
    spec = entry['spec'].format(**values)
    env = {'ORIGIN': args.origin, 'STATE': args.state, 'HOST': args.host, 'BUILD': args.build}  # PW_CHROMIUM and PW_MODULE come from the caller
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


def ssh(host, command, stdin=None, timeout=300):
    return run(['ssh', '-o', 'BatchMode=yes', host, command], stdin=stdin, timeout=timeout)


# Runs on the host: the scratch profile (HOME, XDG dirs, file credentials) and a pb wrapper that mints enrollment commands for the
# fresh account only. The real pb of the host is just the binary; it is never signed in to anything here.
PROFILE = """import json, os, sys
s = sys.argv[1]
try:
    server = {'server_url': json.load(open(os.path.expanduser('~/.config/paperboat/config.json')))['server_url']}
except OSError:
    server = {}  # no config on the host (a fresh CI VM): the official binary's own server
json.dump({**server, 'auth': {'allow_file_fallback': True, 'profile_dir': s + '/credentials'}}, open(s + '/home/config.json', 'w'))
open(s + '/mint.sh', 'w').write('#!/bin/bash\\nexport HOME=%s/home XDG_CONFIG_HOME=%s/config XDG_DATA_HOME=%s/data XDG_STATE_HOME=%s/state XDG_CACHE_HOME=%s/cache\\nexec %s --config %s/home/config.json "$@"\\n' % (s, s, s, s, s, os.path.expanduser('~/.local/bin/pb'), s))
os.chmod(s + '/mint.sh', 0o700)
"""


class Fresh:
    """The run's own accounts and machines: two throwaway accounts (the owner and a teammate), each with a scratch pb on the host signed
    in to it (FRESH_MINT), the fixtures (a paid team owned by the first, the second a member), and four fresh containers."""

    def __init__(self, args):
        self.args, self.stamp = args, str(int(time.time()))
        self.accounts = [('/tmp/final-fresh-%s-%s' % (self.stamp, n), '/tmp/pbfresh-%s-%s' % (self.stamp, n)) for n in ('a', 'b')]  # (work dir, scratch dir on the host)
        self.state = self.accounts[0][0] + '/state.json'
        self.version = ''

    def node(self, script, work, *rest):
        code, out, _ = run(['node', str(HERE / script)] + list(rest), {'ORIGIN': self.args.origin, 'STATE': work + '/state.json'}, timeout=300)
        if code:
            raise RuntimeError('%s %s failed: %s' % (script, ' '.join(rest[:1]), out.strip()[-300:]))
        return out

    def email(self, n):
        return json.load(open(self.accounts[n][0] + '/account.json'))['email']

    def fixtures(self, *extra):
        """final_fixtures.py (Server lane; an empty database has no paid team): a team with a paid entitlement owned by the first account, the
        second a member. Prints one JSON line {"env": {...}} whose variables every check inherits; --teardown makes the members inactive again."""
        if self.args.no_fixtures:
            return {}
        code, out, _ = run([sys.executable, str(HERE / 'final_fixtures.py'), '--email', self.email(0), '--member', self.email(1), *extra], timeout=900)
        if code:
            raise RuntimeError('final_fixtures.py %s failed: %s' % (' '.join(extra), out.strip()[-300:]))
        return {} if extra else json.loads(out.strip().splitlines()[-1]).get('env', {})

    def sign_up(self, n):
        """A new account, and on the host a scratch pb profile signed in to it by API token; returns the path of its mint wrapper there."""
        work, scratch = self.accounts[n]
        self.node('fresh_account.mjs', work, 'signup', work)
        token = self.node('mint_api_token.mjs', work, 'final-recheck')
        code = ssh(self.args.host, 'umask 077; rm -rf {s}; mkdir -p {s}/home {s}/config {s}/data {s}/state {s}/cache; cat > {s}/token'.format(s=scratch), stdin=token)[0]
        code = code or ssh(self.args.host, 'python3 - ' + scratch, stdin=PROFILE)[0]
        login = 'for i in 1 2 3; do {s}/mint.sh login --api-token-file - < {s}/token >/dev/null && {s}/mint.sh auth status --json | grep -q signed_in..true && exit 0; sleep 10; done; exit 1'
        code = code or ssh(self.args.host, login.format(s=scratch))[0]
        if code:
            raise RuntimeError("could not sign the host's scratch pb in to the fresh account")
        return scratch + '/mint.sh'

    def up(self):
        host = self.args.host
        mints = [self.sign_up(0), self.sign_up(1)]
        os.environ['FRESH_MINT'] = mints[0]
        if os.environ.get('FINAL_FIXTURES_URL') and os.environ.get('FINAL_FIXTURES_TOKEN'):  # the tunnel and domain checks create their DNS records through it
            token_file = self.accounts[0][0] + '/fixtures-token'
            Path(token_file).write_text(os.environ['FINAL_FIXTURES_TOKEN'])
            os.chmod(token_file, 0o600)
            os.environ['FINAL_FIXTURES_TOKEN_FILE'] = token_file
            request = urllib.request.Request(os.environ['FINAL_FIXTURES_URL'].rstrip('/') + '/zone', headers={'Authorization': 'Bearer ' + os.environ['FINAL_FIXTURES_TOKEN']})
            with urllib.request.urlopen(request, timeout=30) as reply:
                os.environ['SMOKE_DNS_ZONE'] = json.load(reply)['data']['zone']
        if not os.environ.get('SMOKE_SERVER_VERSION'):  # tunnel-command rows check against the deployed server's version (the console proxies /api/v1)
            with urllib.request.urlopen(self.args.origin + '/api/v1/server-version', timeout=30) as reply:
                os.environ['SMOKE_SERVER_VERSION'] = json.load(reply)['data']['version']
        os.environ.update(self.fixtures())
        code, out, secs = run([str(HERE / 'final_fresh.sh'), 'up', host, self.stamp, mints[1]], timeout=3000)
        versions = set(re.findall(r'^OFFICIAL_\S+=(\S+)$', out, re.M))
        if code or len(versions) != 1:
            raise RuntimeError('the fresh machines did not come up (exit %d): %s' % (code, ' '.join(out.strip().splitlines()[-3:])[:300]))
        self.version = versions.pop()
        if self.args.build != 'latest' and self.version != self.args.build:
            raise RuntimeError('the published one-liner installed %s, not the release under test %s' % (self.version, self.args.build))
        self.args.ct_map = 'fx-a=%s:final-a-%s,fx-b=%s:final-b-%s' % (host, self.stamp, host, self.stamp)
        self.args.state, self.args.version = self.state, self.version
        # F9 (local-access, private-access-permission): owner and controller are container A, the team the fixture's, the accessor the teammate's machine
        os.environ.update({'F9_CONTROLLER': 'final-a-' + self.stamp, 'F9_TARGET': 'fx-b', 'F9_OWNER': 'final-a-' + self.stamp, 'F9_OWNER_ALIAS': 'fx-a',
                           'F9_ACCESSOR': 'final-d-' + self.stamp, 'F9_TEAM': os.environ.get('PAPERBOAT_SMOKE_TEAM', ''), 'CT_SSH_HOST': host})
        return secs

    def down(self):
        host, notes = self.args.host, []
        run([str(HERE / 'final_fresh.sh'), 'down', host, self.stamp])
        try:
            if os.path.exists(self.accounts[0][0] + '/account.json') and os.path.exists(self.accounts[1][0] + '/account.json'):
                self.fixtures('--teardown')
        except (RuntimeError, OSError) as error:
            notes.append('the fixtures were NOT torn down: ' + str(error))
        for work, scratch in self.accounts:
            deleted = not os.path.exists(work + '/account.json')
            if not deleted:
                try:
                    self.node('fresh_account.mjs', work, 'delete', work)
                    deleted = True
                except RuntimeError as error:
                    notes.append('a throwaway account was NOT deleted (its login stays in %s so it can be retried): %s' % (work, error))
            ssh(host, 'rm -rf ' + scratch)
            if deleted:
                run(['rm', '-rf', work])
        return notes


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
    if args.skip_go:
        tests = set()
    if args.shard:  # I/N: this VM takes every N-th check (smoke ids by group, so a group is never split)
        index, count = (int(x) for x in args.shard.split('/'))
        keys = sorted({'smoke:' + i.partition(':')[0] for i in ids} | {'go:' + t for t in tests} | {e['check'] for e in todo})
        take = {k for pos, k in enumerate(keys) if pos % count == index - 1}
        ids = {i for i in ids if 'smoke:' + i.partition(':')[0] in take}
        tests = {t for t in tests if 'go:' + t in take}
        todo = [e for e in todo if e['check'] in take]
    if args.dry:
        print('smoke ids:', sorted(ids)); print('go tests:', len(tests)); print('entries:', [e['check'] for e in todo])
        print('machines: %s' % ('fresh containers on %s in two fresh throwaway accounts, one-liner %s' % (args.host, args.build) if ids or any(e['kind'] != 'go' for e in todo) else 'none needed'))
        return 0
    results = []
    fresh = Fresh(args)
    needs_machines = bool(ids) or any(e['kind'] != 'go' for e in todo)
    try:
        if needs_machines:
            secs = fresh.up()
            if (not wanted or 'EXTRA' in wanted or 'extra-published-install' in wanted) and (not args.shard or args.shard.startswith('1/')):
                results.append({'check': 'extra-published-install', 'kind': 'cmd', 'status': 'PASS', 'seconds': secs,
                                'detail': 'fresh containers installed with the published one-liner: pb %s, enrolled, pair usable' % fresh.version})
        if ids:
            run_smoke(ids, args, results)
        if tests:
            run_go(sorted(tests), results)
        for entry in todo:
            if '{previous}' in entry['spec'] and not args.previous:
                results.append({'check': entry['check'], 'kind': entry['kind'], 'status': 'FAIL', 'seconds': 0, 'detail': 'pass --previous <the previous official release>'})
            else:
                run_entry(entry, args, results)
    except RuntimeError as error:
        results.append({'check': 'fresh-machines', 'kind': 'setup', 'status': 'FAIL', 'seconds': 0, 'detail': str(error)[:300]})
    finally:
        if args.keep:
            print('kept: account dirs %s, containers final-{a,b,c,d}-%s on %s' % (' '.join(w for w, _ in fresh.accounts), fresh.stamp, args.host))
        else:
            for note in fresh.down():
                results.append({'check': 'fresh-teardown', 'kind': 'setup', 'status': 'FAIL', 'seconds': 0, 'detail': note[:300]})
    return report(args.build, chosen, results)


def report(build, chosen, results):
    """Writes results/final-recheck-<build>-<UTC>.{md,json} and prints the table; the exit status is 1 when any check failed."""
    by_check = {}
    for r in results:  # a check run by several shards counts as failed if any run failed
        if r['check'] not in by_check or r['status'] == 'FAIL':
            by_check[r['check']] = r
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    out_dir = HERE / 'results'
    out_dir.mkdir(exist_ok=True)
    per_row = []
    for r in chosen:
        mine = ['smoke:' + i.partition(':')[0] for i in r['smoke']] + ['go:%s/%s' % (go_tests()[t], t) for t in r['go']] + r['declared']
        got = [by_check[c]['status'] for c in mine if c in by_check]
        status = 'FAIL' if 'FAIL' in got else 'PASS' if got else ('NO CHECK' if r['state'] == '[x]' else r['state'])
        per_row.append((r['row'], r['state'], status, len(got)))
    md = ['# Final Linux re-check %s (%s)' % (build, stamp), '', '| check | kind | status | s | detail |', '|---|---|---|---|---|']
    md += ['| %s | %s | %s | %d | %s |' % (r['check'], r['kind'], r['status'], r['seconds'], r['detail'].replace('|', '/')) for r in results]
    md += ['', '| row | Linux | result | checks run |', '|---|---|---|---|'] + ['| %s | %s | %s | %d |' % x for x in per_row]
    (out_dir / ('final-recheck-%s-%s.md' % (build, stamp))).write_text('\n'.join(md) + '\n')
    (out_dir / ('final-recheck-%s-%s.json' % (build, stamp))).write_text(json.dumps({'build': build, 'checks': results, 'rows': per_row}, indent=1))
    print('\n'.join(md))
    return 1 if any(r['status'] == 'FAIL' for r in results) else 0


def cmd_merge(args):
    """One table from the shards' result files (the JSON each shard wrote)."""
    results = [c for path in args.files for c in json.loads(Path(path).read_text())['checks']]
    return report(args.build, inventory(), results)


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest='command', required=True)
    inv = sub.add_parser('inventory')
    inv.add_argument('--out')
    go = sub.add_parser('run')
    go.add_argument('--build', required=True)
    go.add_argument('--host', default='hp')
    go.add_argument('--previous', default='2026.10.27.0', help='the previous official release the update-with-history extra starts from (the update goes to the Latest)')
    go.add_argument('--origin', default=os.environ.get('PB_CONSOLE_ORIGIN', ''))
    go.add_argument('--keep', action='store_true')
    go.add_argument('--shard', default='', help='I/N: run every N-th check on this machine (the other shards run elsewhere; merge joins them)')
    go.add_argument('--skip-go', action='store_true', help='leave out the Go tests (they test the commit, not a build; tools/ci-test.sh runs them)')
    go.add_argument('--no-fixtures', action='store_true', help='dry runs only: do not create the fixtures team (checks needing a team fail)')
    merge = sub.add_parser('merge')
    merge.add_argument('--build', required=True)
    merge.add_argument('files', nargs='+')
    go.add_argument('--only', default='')
    go.add_argument('--dry', action='store_true')
    args = parser.parse_args()
    if args.command == 'inventory':
        return cmd_inventory(args)
    if args.command == 'merge':
        return cmd_merge(args)
    return cmd_run(args)


if __name__ == '__main__':
    sys.exit(main() or 0)
