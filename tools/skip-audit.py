#!/usr/bin/env python3
"""No silent skips: fail when a Go test skipped in every step of a CI run and ran in none.

A test that skips in one step (no PostgreSQL DSN, not root, no PgBouncer) is fine when another step of the
same run executes it (pass or fail). A test that only ever skipped must match a line of the allowlist,
which names the job that runs it instead (another workflow, the device smoke suite); otherwise this fails.

Usage: skip-audit.py ALLOWLIST BASELINE EVENTS.jsonl...   (files written by tools/gotest-json.py --save)
Allowlist lines: `REGEX<TAB>RUNS IN`, matched against "package Test/sub: skip reason". # starts a comment.
Baseline lines: `package Test/sub`, the never-run tests known when the audit started. It is a ratchet: a new
never-run test fails, and so does a baselined test that runs now (remove its line), so the list only shrinks.
"""
import json
import re
import sys


def load_allowlist(path):
    entries = []
    for number, line in enumerate(open(path), 1):
        line = line.rstrip('\n')
        if not line.strip() or line.lstrip().startswith('#'):
            continue
        pattern, sep, runs_in = line.partition('\t')
        if not sep or not runs_in.strip():
            raise SystemExit('%s:%d: an allowlist line needs REGEX<TAB>the job that runs the test' % (path, number))
        entries.append((re.compile(pattern), runs_in.strip()))
    return entries


def skip_reason(lines):
    """The t.Skip message: the output lines between the test's RUN and its --- SKIP line."""
    kept = [l.strip() for l in lines if l.strip() and not l.lstrip().startswith(('=== ', '--- '))]
    return ' '.join(kept)[:200]


def load_baseline(path):
    return {line.strip() for line in open(path) if line.strip() and not line.lstrip().startswith('#')}


def audit(allowlist, files):
    ran, skipped, output = set(), {}, {}
    for path in files:
        for line in open(path):
            try:
                event = json.loads(line)
            except ValueError:
                continue
            test = event.get('Test')
            if not test:
                continue
            key = (event.get('Package', ''), test)
            if event.get('Action') == 'output':
                output.setdefault(key, []).append(event.get('Output', ''))
            elif event.get('Action') in ('pass', 'fail'):
                ran.add(key)
            elif event.get('Action') == 'skip':
                skipped[key] = skip_reason(output.get(key, []))
            if event.get('Action') in ('pass', 'fail', 'skip'):
                output.pop(key, None)
    silent = {}
    for key, reason in sorted(skipped.items()):
        if key in ran:
            continue
        subject = '%s %s: %s' % (key[0], key[1], reason)
        if not any(pattern.search(subject) for pattern, _ in allowlist):
            silent['%s %s' % key] = subject
    return silent


def main():
    if len(sys.argv) < 4:
        raise SystemExit(__doc__)
    allowlist_path, baseline_path, files = sys.argv[1], sys.argv[2], sys.argv[3:]
    silent, baseline = audit(load_allowlist(allowlist_path), files), load_baseline(baseline_path)
    new = [subject for key, subject in silent.items() if key not in baseline]
    stale = sorted(baseline - set(silent))
    for subject in new:
        print('SILENT SKIP ' + subject)
    for key in stale:
        print('BASELINE STALE %s: it runs (or no longer skips) now; delete its line' % key)
    print('skip audit: %d never-run test(s) in the baseline (%s)' % (len(baseline & set(silent)), baseline_path))
    if new:
        print('%d new test(s) skipped in every step and ran nowhere: run them in a CI step, or add an allowlist '
              'line naming the job that runs them (%s)' % (len(new), allowlist_path))
    if stale:
        print('%d baseline line(s) are done: delete them so the list only shrinks' % len(stale))
    if new or stale:
        sys.exit(1)


if __name__ == '__main__':
    main()
