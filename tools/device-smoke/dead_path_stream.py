#!/usr/bin/env python3
"""F21-160 device check: a dead direct path is noticed in seconds and a running stream recovers over the relay.

  dead_path_stream.py PEER [--peer-address ADDRESS] [--bound 12]   (run on a paired Linux or macOS machine; needs sudo -n iptables or pfctl)
  Without ADDRESS this machine's UDP is dropped (path_display.udp_blocked); with it only packets to that host address of PEER are dropped
  (as direct_path_cut.py does on Windows), which kills the direct path only when it runs to that one address.
  1. `pb ping PEER` until `pb status` says the selected path to PEER is direct;
  2. a stream starts: `pb exec PEER -- sh -c 'echo <n>; sleep 0.1'` in a loop, one line every 0.1 s, each arrival timestamped here;
  3. the direct path is killed by the block above (iptables, or a pf anchor on macOS; TCP stays up so the relay and the control plane are reachable);
  4. the longest gap between two lines after the block is at most BOUND seconds (the unit test TestDeadDirectPathFalls-
     BackToTheRelayInSeconds measures 6.5 s, bound 12 s), the stream goes on to the end with every number in order,
     and while the block lasts `pb status` names a relay path (wss or relay), not direct.
Prints one JSON line {"check": "dead-path-stream", ...}; exit 1 on failure.
"""
import json
import subprocess
import sys
import threading
import time

import path_display as pd
from relay_home_wire import blocked
from pairing_lifecycle import command_result  # noqa: F401  (path_display's pb() uses it)


def selected(peer):
    return pd.machine_status(peer)['selected_path']


def main():
    peer = sys.argv[1]
    bound = float(sys.argv[sys.argv.index('--bound') + 1]) if '--bound' in sys.argv else 12.0
    address = sys.argv[sys.argv.index('--peer-address') + 1] if '--peer-address' in sys.argv else None
    for _ in range(45):  # a path takes seconds to settle after a start
        pd.pb('ping', peer)
        if selected(peer) == 'direct':
            break
        time.sleep(2)
    else:
        raise RuntimeError('no direct path to %s to kill (selected %s)' % (peer, selected(peer)))
    total = 900
    script = 'i=0; while [ $i -lt %d ]; do echo $i; i=$((i+1)); sleep 0.1; done' % total
    stream = subprocess.Popen([str(pd.PB), 'exec', peer, '--', 'sh', '-c', script], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                              stdin=subprocess.DEVNULL, bufsize=0)
    arrivals = []

    def read():
        for line in iter(stream.stdout.readline, b''):
            arrivals.append((time.time(), line.strip()))
    reader = threading.Thread(target=read, daemon=True)
    reader.start()
    started = time.time()
    while len(arrivals) < 30 and time.time() - started < 60:
        time.sleep(0.5)
    if len(arrivals) < 30:
        raise RuntimeError('the stream did not start: %d lines in 60 s' % len(arrivals))
    during = None
    with (blocked(address) if address else pd.udp_blocked()):
        blocked_at = time.time()
        deadline = blocked_at + 120
        while stream.poll() is None and time.time() < deadline and len(arrivals) < total:
            time.sleep(1)
            if time.time() - blocked_at > 40 and time.time() - arrivals[-1][0] < 3:
                break  # the stream is flowing again; give it a moment of steady flow, then release
        time.sleep(5)
        during = selected(peer)
    after = [t for t, _ in arrivals if t >= blocked_at]
    if len(after) < 20:
        raise RuntimeError('the stream did not recover after the direct path to the peer was dropped: %d lines after the block' % len(after))
    gaps = [b - a for a, b in zip([blocked_at] + after, after)]
    longest = max(gaps)
    stream.terminate()
    numbers = [int(v) for _, v in arrivals if v.isdigit()]
    if numbers != list(range(len(numbers))):
        raise RuntimeError('the stream lost or repeated lines around the path change')
    if during == 'direct':
        raise RuntimeError('the stream flowed again but the path is still direct while the block lasts')
    if longest > bound:
        raise RuntimeError('the longest silence after the block was %.1f s (bound %.0f s)' % (longest, bound))
    print(json.dumps({'check': 'dead-path-stream', 'status': 'PASS', 'longest_gap_s': round(longest, 1), 'bound_s': bound,
                      'lines_before': len([t for t, _ in arrivals if t < blocked_at]), 'lines_after': len(after),
                      'path_while_blocked': during, 'in_order': True}))


if __name__ == '__main__':
    try:
        main()
    except Exception as err:  # noqa: BLE001
        print(json.dumps({'check': 'dead-path-stream', 'status': 'FAIL', 'detail': str(err)}))
        sys.exit(1)
