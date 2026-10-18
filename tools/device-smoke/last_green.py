#!/usr/bin/env python3
"""Report each regression's exact last passing receipt and candidate history."""
import argparse
import datetime as dt
import json
from pathlib import Path
import re
import shlex
import subprocess

ROOT = Path(__file__).resolve().parents[2]
PATHS = ['cli/', 'relay/', 'shared/']


def git(*args, root=ROOT):
    return subprocess.check_output(['git', '-C', str(root), *args], text=True,
                                   stderr=subprocess.DEVNULL, timeout=15).strip()


def timestamp(path, receipt):
    value = str(receipt.get('at') or path.stem)
    for pattern in ('%Y%m%dT%H%M%SZ', '%Y-%m-%dT%H:%M:%SZ'):
        try:
            return dt.datetime.strptime(value, pattern).replace(tzinfo=dt.timezone.utc)
        except ValueError:
            pass
    return dt.datetime.fromisoformat(value.replace('Z', '+00:00')).astimezone(dt.timezone.utc)


def receipts(directory):
    for path in sorted(directory.glob('*.json')):
        try:
            receipt = json.loads(path.read_text())
            yield timestamp(path, receipt), path, receipt
        except (ValueError, TypeError, OSError):
            continue


def last_pass(directory, check, before):
    matches = [(at, path, data) for at, path, data in receipts(directory)
               if at < before and any(isinstance(row, dict) and row.get('id') == check and row.get('status') == 'PASS'
                                      for row in data.get('results', []))]
    return max(matches, key=lambda item: (item[0], item[1].name), default=None)


def build_commit(receipt, root=ROOT):
    version = receipt.get('expect_version', '')
    candidate = receipt.get('build_commit') or (version[4:] if version.startswith('dev-') else '')
    candidate = candidate or receipt.get('source_commit', '')
    if not re.fullmatch(r'[a-fA-F0-9]{7,40}', candidate):
        return None
    try:
        return git('rev-parse', '--verify', candidate + '^{commit}', root=root)
    except subprocess.SubprocessError:
        return None


def deploys(root, start, end):
    """PLAN is the existing deployment log; retain its dated entries, never invent deploy times."""
    plan = root / 'PLAN.md'
    if not plan.exists():
        return []
    entries = []
    for number, line in enumerate(plan.read_text().splitlines(), 1):
        day = re.match(r'^(\d{4}-\d{2}-\d{2})\s*\|', line)
        if not day or not (start - dt.timedelta(days=1)).date().isoformat() <= day[1] <= (end + dt.timedelta(days=1)).date().isoformat():
            continue
        if re.search(r'\bserver(?=\b|[0-9])', line, re.I) and re.search(r'\bdeployed\b', line, re.I):
            position = line.lower().index('deployed')
            excerpt = line[max(0, position-90):position+130]
            entries.append(f'PLAN.md:{number} {day[1]} …{excerpt}…')
    return entries


def report(result, directory, root=ROOT, check=None):
    current = json.loads(result.read_text())
    before = timestamp(result, current)
    failing = [check] if check else sorted({row['id'] for row in current.get('results', [])
                                           if row.get('status') == 'FAIL'})
    lines = []
    for identity in failing:
        row = next((row for row in current.get('results', []) if row.get('id') == identity), {})
        if row.get('triage') == 'NETWORK' and row.get('failure_code') == 'network_dns':
            lines.append(f'NETWORK {identity}: network-inconclusive; FAIL retained, no product regression range established.')
            devices = list(dict.fromkeys(re.findall(r'(?<![a-z])(hp|apple|victus)(?![a-z])', identity.split(':')[-1])))
            command = ['tools/device-smoke/smoke.sh', '--expect-version', current.get('expect_version', 'BUILD_VERSION'),
                       '--targets', ','.join(devices or ['hp', 'apple']), '--only-check', identity]
            lines.append('Record one PLAN Log line and rerun this exact check once: ' + shlex.join(command))
            continue
        previous = last_pass(directory, identity, before)
        if previous is None:
            lines.append(f'{identity}: no earlier PASS receipt; no regression range established.')
            continue
        at, path, data = previous
        commit = build_commit(data, root)
        lines.append(f'REGRESSION {identity}: last PASS {path.name} ({data.get("expect_version", "unknown build")})')
        if not commit:
            lines.append('Build commit unavailable; preserve the original build before choosing a bisect range.')
            continue
        head = git('rev-parse', 'HEAD', root=root)
        lines.extend([f'Build commit: {commit}', f'Candidate range: {commit}..{head}'])
        history = git('log', '--oneline', commit + '..' + head, '--', *PATHS, root=root)
        lines.append(history or '(no cli/relay/shared commits in this range)')
        lines.append('First rerun this single check on the last passing build against the current server.')
        lines.append(shlex.join(['git', 'bisect', 'start', head, commit, '--', *PATHS]))
        devices = list(dict.fromkeys(re.findall(r'(?<![a-z])(hp|apple|victus)(?![a-z])', identity.split(':')[-1])))
        command = ['tools/device-smoke/smoke.sh', '--expect-version', 'BUILD_VERSION',
                   '--targets', ','.join(devices or ['hp', 'apple']), '--only-check', identity]
        lines.append('At each bisect step, build/install that committed revision, then: ' + shlex.join(command))
        lines.append('Server versions in receipts: ' + str(data.get('server', 'not recorded')) + ' -> ' + str(current.get('server', 'not recorded')))
        entries = deploys(root, at, before)
        lines.append('Server deployment log (date precision, adjacent dates included for timezone uncertainty; verify exact deployment times):')
        lines.extend(entries or ['(no deployment evidence recorded for these dates)'])
    return '\n'.join(lines)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('result', type=Path)
    parser.add_argument('--results-dir', type=Path)
    parser.add_argument('--check')
    args = parser.parse_args()
    try:
        print(report(args.result, args.results_dir or args.result.parent, check=args.check))
    except (OSError, ValueError, subprocess.SubprocessError):
        parser.exit(2, 'Last-green report unavailable; original smoke failure is unchanged.\n')


if __name__ == '__main__':
    main()
