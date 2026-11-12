#!/usr/bin/env python3
"""PR-06: one tunnel stream through the edge sustains >= 150 Mbit/s at 50 ms RTT.

Run on the machine that serves the origin (a disposable container with sudo and tc), the budget's RTT is added with netem on
this machine's egress (its carrier to the edge), and a second machine of the account downloads the tunnel's URL.
  tunnel_throughput.py DOWNLOADER [--mib 200] [--rtt-ms 50] [--runs 3] [--budget-mbit 150]
Creates one tunnel and one 200 MiB file, removes both and the netem rule at the end (and a systemd timer removes the rule
after ten minutes if this process dies). Prints one JSON receipt (numbers only).
"""
import argparse
import contextlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import uuid

from install_lifecycle import registration
from tunnel_lifecycle import confirmed, run, wait

PB = Path.home() / '.local/bin/pb'
PORT = 18099


def sh(*command, timeout=120):
    return subprocess.run(command, capture_output=True, text=True, timeout=timeout)


@contextlib.contextmanager
def netem(rtt_ms):
    """Delay this machine's egress by rtt_ms: its carrier's data then sees that RTT to the edge (acks come back at once)."""
    interface = sh('sh', '-c', "ip -o route get 1.1.1.1 | sed -n 's/.* dev \\([^ ]*\\).*/\\1/p'").stdout.strip()
    assert interface, 'no default interface'
    sh('sudo', '-n', 'systemd-run', '--quiet', '--on-active=600', 'tc', 'qdisc', 'del', 'dev', interface, 'root')
    done = sh('sudo', '-n', 'tc', 'qdisc', 'replace', 'dev', interface, 'root', 'netem', 'delay', f'{rtt_ms}ms')
    assert done.returncode == 0, 'netem failed: ' + done.stderr[-200:]
    try:
        yield interface
    finally:
        sh('sudo', '-n', 'tc', 'qdisc', 'del', 'dev', interface, 'root')


def download(downloader, url, seconds):
    """curl on the downloader machine through `pb exec`: bytes, seconds, Mbit/s."""
    result = subprocess.run([str(PB), 'exec', downloader, '--timeout', f'{seconds}s', '--', 'curl', '-s', '-o', '/dev/null', '-w',
                             '%{size_download} %{time_total} %{http_code}', url],
                            capture_output=True, text=True, timeout=seconds + 30)
    size, took, code = result.stdout.split()[-3:]
    return int(size), float(took), code


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('downloader')
    parser.add_argument('--mib', type=int, default=200)
    parser.add_argument('--rtt-ms', type=int, default=50)
    parser.add_argument('--runs', type=int, default=3)
    parser.add_argument('--budget-mbit', type=float, default=150)
    args = parser.parse_args()
    before = registration()
    name = 'smoke-tunnel-thru-' + uuid.uuid4().hex[:10]
    identifier = None
    runs = []
    with tempfile.TemporaryDirectory() as root:
        with open(os.path.join(root, 'big'), 'wb') as handle:
            for _ in range(args.mib):
                handle.write(os.urandom(1 << 20))
        origin = subprocess.Popen(['python3', '-m', 'http.server', str(PORT), '--bind', '127.0.0.1', '--directory', root],
                                  stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            with netem(args.rtt_ms) as interface:
                # The carrier's stream window grows from the RTT its yamux session measured (pings from session start); a session
                # that connected before the delay existed keeps measuring ~0 ms for a while, so the carrier is restarted under it.
                sh('sh', '-c', 'sudo -n systemctl restart "paperboat-carrier-u$(id -u)-*.service"')
                time.sleep(15)
                ping = sh('sh', '-c', "ping -c 3 -q $(ip route | sed -n 's/default via \\([^ ]*\\).*/\\1/p') | tail -1").stdout.strip()
                created = run('tunnel', 'create', name, '--port', str(PORT), '--duration', '30m', '--wait', '--timeout', '90s')
                identifier = created['tunnel']['id']
                url = created['tunnel']['stable_endpoint'] + '/big'
                wait(lambda: download(args.downloader, url, 120)[2] == '200', 'tunnel to serve the file', 180)
                for _ in range(args.runs):
                    size, took, code = download(args.downloader, url, 300)
                    runs.append({'bytes': size, 'seconds': round(took, 2), 'http': code,
                                 'mbit_s': round(size * 8 / took / 1e6, 1) if took else 0})
        finally:
            origin.terminate()
            if identifier is not None:
                confirmed('tunnel', 'delete', identifier)
    good = sorted(r['mbit_s'] for r in runs if r['http'] == '200' and r['bytes'] == args.mib << 20)
    median = good[len(good) // 2] if len(good) == args.runs else 0  # a failed run fails the check
    ok = median >= args.budget_mbit and registration() == before
    print(json.dumps({'check': 'tunnel-throughput', 'status': 'PASS' if ok else 'FAIL', 'rtt_ms': args.rtt_ms, 'netem_dev': interface,
                      'ping_to_gateway': ping, 'runs': runs, 'median_mbit_s': median, 'budget_mbit_s': args.budget_mbit,
                      'enrollment_unchanged': registration() == before}))
    raise SystemExit(0 if ok else 1)


if __name__ == '__main__':
    main()
