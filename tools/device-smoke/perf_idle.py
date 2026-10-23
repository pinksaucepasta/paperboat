#!/usr/bin/env python3
"""Idle cost of one machine over an interval, checked against perf_budgets.BUDGETS['idle'] (PR-03).

Run on the machine itself while nothing uses it. Measures the CPU time and context switches
(wakeups) of every `pb` process from /proc, and the TCP bytes sent and received on port 443
(control plane and edge) from two `ss -tni` snapshots. Prints one JSON receipt with numbers only.
Usage: perf_idle.py [--seconds 600] [--no-budget]
"""
import argparse
import json
import os
import re
import subprocess
import time

from perf_budgets import BUDGETS


def pb_processes():
    """pid -> (cpu seconds, context switches) for every pb process."""
    out = {}
    for pid in filter(str.isdigit, os.listdir('/proc')):
        try:
            argv0 = open(f'/proc/{pid}/cmdline').read().split('\0')[0]
            if not argv0.endswith('/pb'):
                continue
            stat = open(f'/proc/{pid}/stat').read().rsplit(')', 1)[1].split()
            cpu = (int(stat[11]) + int(stat[12])) / os.sysconf('SC_CLK_TCK')
            switches = 0
            for task in os.listdir(f'/proc/{pid}/task'):
                for line in open(f'/proc/{pid}/task/{task}/status'):
                    if line.startswith(('voluntary_ctxt_switches', 'nonvoluntary_ctxt_switches')):
                        switches += int(line.split(':')[1])
            out[pid] = (cpu, switches)
        except (OSError, IndexError, ValueError):
            continue
    return out


def tls_bytes():
    """(local, remote) -> bytes sent + received for established TCP connections to port 443."""
    lines = subprocess.run(['ss', '-tni'], capture_output=True, text=True, timeout=30).stdout.splitlines()
    out, key = {}, None
    for line in lines:
        if line.startswith('ESTAB'):
            parts = line.split()
            key = (parts[3], parts[4]) if parts[4].endswith(':443') else None
        elif key:
            counters = dict(re.findall(r'\b(bytes_sent|bytes_received):(\d+)', line))
            out[key] = int(counters.get('bytes_sent', 0)) + int(counters.get('bytes_received', 0))
            key = None
    return out


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--seconds', type=int, default=600)
    parser.add_argument('--no-budget', action='store_true', help='measure only')
    args = parser.parse_args()
    procs_before, bytes_before = pb_processes(), tls_bytes()
    time.sleep(args.seconds)
    procs_after, bytes_after = pb_processes(), tls_bytes()
    cpu = sum(after[0] - procs_before[pid][0] for pid, after in procs_after.items() if pid in procs_before)
    switches = sum(after[1] - procs_before[pid][1] for pid, after in procs_after.items() if pid in procs_before)
    # A connection opened during the interval counts from zero; one closed during it is not seen.
    transferred = sum(total - bytes_before.get(key, 0) for key, total in bytes_after.items())
    hour = 3600 / args.seconds
    metrics = {'cpu_percent': round(cpu / args.seconds * 100, 2), 'wakeups_per_s': round(switches / args.seconds, 1),
               'tls_mb_per_hour': round(transferred * hour / 1e6, 2), 'pb_processes': len(procs_after),
               'seconds': args.seconds}
    budget = BUDGETS.get('idle', {})
    failures = [] if args.no_budget else [
        name for name, (kind, bound) in budget.items()
        if (metrics[name] > bound if kind == 'max' else metrics[name] < bound)]
    if not budget and not args.no_budget:
        failures = ['no idle budget']
    print(json.dumps({'status': 'FAIL' if failures else 'PASS', 'check': 'perf-idle', 'metrics': metrics,
                      'budget_failures': failures}))


if __name__ == '__main__':
    main()
