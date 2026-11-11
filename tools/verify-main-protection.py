#!/usr/bin/env python3
"""R-01 live negative check: an empty, unreviewed commit must be refused by main.

Creates only a local commit object. A rejected push is success; an accepted push fails
this check. Run after setting protection, with a clean tree and repository admin access.
"""
import argparse
import json
import re
import subprocess
from pathlib import Path


def output(*args):
    return subprocess.check_output(args, text=True).strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', required=True)
    args = parser.parse_args()
    repo = output('gh', 'repo', 'view', '--json', 'nameWithOwner', '--jq', '.nameWithOwner')
    protection = json.loads(output('gh', 'api', f'repos/{repo}/branches/main/protection'))
    queues = json.loads(output('gh', 'api', f'repos/{repo}/rulesets'))
    protected = (protection['enforce_admins']['enabled'] and
                 protection.get('required_pull_request_reviews') is not None and
                 bool(protection['required_status_checks']['checks']) and
                 protection['required_linear_history']['enabled'] and
                 not protection['allow_force_pushes']['enabled'])
    if not protected:
        raise SystemExit('refusing the negative push probe: main is not protected')
    subprocess.run(['git', 'fetch', '-q', 'origin', 'main'], check=True, timeout=60)
    base = output('git', 'rev-parse', 'origin/main')
    tree = output('git', 'rev-parse', base + '^{tree}')
    commit = output('git', 'commit-tree', tree, '-p', base, '-m', 'Verify protected main rejects an unreviewed empty commit')
    attempt = subprocess.run(['git', 'push', 'origin', commit + ':refs/heads/main'],
                             capture_output=True, text=True, timeout=120)
    # No raw remote diagnostics: credentials may be present in a remote URL.
    refusal = re.findall(r'GH00[0-9]|GH01[0-9]', attempt.stderr)
    result = {'base': base, 'probe': commit, 'same_tree': tree == output('git', 'rev-parse', commit + '^{tree}'),
              'admins': protection['enforce_admins']['enabled'], 'strict': protection['required_status_checks']['strict'],
              'required_checks': [c['context'] for c in protection['required_status_checks']['checks']],
              'queue_active': any(q['name'] == 'Paperboat main merge queue' and q['enforcement'] == 'active' for q in queues),
              'direct_push_exit': attempt.returncode, 'refusal_codes': sorted(set(refusal))}
    result['ok'] = result['queue_active'] and attempt.returncode != 0 and bool(refusal)
    Path(args.output).write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps({'ok': result['ok'], 'direct_push_exit': attempt.returncode, 'refusal_codes': result['refusal_codes']}))
    return 0 if result['ok'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
