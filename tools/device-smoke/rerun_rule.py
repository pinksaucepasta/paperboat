#!/usr/bin/env python3
"""A row that failed and then passed on a rerun counts as FAIL until its cause is known.

Only a recorded cause changes how such a row is shown. The cause file holds one line per row, tab separated:
    ROW-ID (or SHARD/ROW-ID)    script|runner|product    cause
A script or runner cause turns the row into an annotated PASS; a product cause stays FAIL.

  rerun_rule.py --results-dir DIR --version V [--run SHARD] [--causes FILE]    (smoke.sh's report)
reads the ids this run passed on stdin and prints `ID<tab>PASS|FAIL<tab>label` for each one an earlier receipt of the
same build in DIR has as failed.
"""
import argparse
import json
from pathlib import Path
import sys

CLASSES = ('script', 'runner', 'product')


def load_causes(path):
    """{key: (class, cause)} from the cause file; no path or no file means no known cause. A bad line is an error."""
    causes = {}
    if not path or not Path(path).exists():
        return causes
    for number, line in enumerate(Path(path).read_text().splitlines(), 1):
        if not line.strip() or line.startswith('#'):
            continue
        fields = [field.strip() for field in line.split('\t')]
        if len(fields) != 3 or not all(fields) or fields[1] not in CLASSES or fields[0] in causes:
            raise ValueError('%s:%d: want one `row<tab>%s<tab>cause` line per row' % (path, number, '|'.join(CLASSES)))
        causes[fields[0]] = (fields[1], fields[2])
    return causes


def cause_for(causes, shard, row):
    return causes.get('%s/%s' % (shard, row)) or causes.get(row)


def verdict(states, cause=None):
    """(counted state, label) of one row from its results, oldest first."""
    last = states[-1]
    if 'FAIL' not in states:
        return last, last
    known = '%s: %s' % cause if cause else ''
    if last != 'PASS':
        notes = [note for note in ('' if last == 'FAIL' else 'then ' + last, known) if note]
        return 'FAIL', 'FAIL (%s)' % '; '.join(notes) if notes else 'FAIL'
    if not cause:
        return 'FAIL', 'FAIL (passed on rerun, cause unknown)'
    state = 'FAIL' if cause[0] == 'product' else 'PASS'
    return state, '%s (passed on rerun, %s)' % (state, known)


def failed_before(directory, version):
    """Ids that an earlier receipt of this build in directory has as FAIL."""
    failed = set()
    for path in sorted(Path(directory).glob('*.json')):
        try:
            receipt = json.loads(path.read_text())
            if not isinstance(receipt, dict) or receipt.get('expect_version') != version:
                continue
            failed.update(row['id'] for row in receipt.get('results', []) if row.get('status') == 'FAIL')
        except (ValueError, TypeError, KeyError, AttributeError, OSError):
            print('rerun rule: unreadable receipt %s' % path.name, file=sys.stderr)
    return failed


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--results-dir', required=True)
    parser.add_argument('--version', required=True)
    parser.add_argument('--run', default='')
    parser.add_argument('--causes', default='')
    args = parser.parse_args(argv)
    causes = load_causes(args.causes)
    before = failed_before(args.results_dir, args.version)
    for row in sys.stdin.read().split():
        if row in before:
            print('%s\t%s\t%s' % ((row,) + verdict(['FAIL', 'PASS'], cause_for(causes, args.run, row))))
    return 0


if __name__ == '__main__':
    sys.exit(main())
