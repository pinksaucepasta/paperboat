#!/usr/bin/env python3
"""F21-160 device check: a dead direct path is noticed in seconds and a running stream recovers over the relay.

  dead_path_stream.py PEER [--peer-address ADDRESS] [--bound 12]   (run on a paired Linux or macOS machine; needs sudo -n iptables or pfctl)
  With PB_SMOKE_WINDOWS_PB (and SMOKE_WINDOWS_SSH, SMOKE_WINDOWS_PASSWORD) the stream client and every `pb status` run ON that Windows device (lines are
  timestamped here as the ssh session delivers them); the cut must keep pb.exe running (pd.udp_blocked() restarts it), so
  SMOKE_PEER_CUT is required (container_cut.sh `silent` drop on the peer's host; repeat --device-address for the address the peer sees the
  device at, as direct_path_cut.py does).
  Without ADDRESS this machine's UDP is dropped (path_display.udp_blocked); with it only packets to that host address of PEER are dropped
  (as direct_path_cut.py does on Windows), which kills the direct path only when it runs to that one address.
  1. `pb ping PEER` until `pb status` says the selected path to PEER is direct;
  2. a stream starts: `pb exec PEER -- sh -c 'echo <n>; sleep 0.1'` in a loop, one line every 0.1 s, each arrival timestamped here;
  3. the direct path is killed by the block above (iptables, or a pf anchor on macOS; TCP stays up so the relay and the control plane are reachable);
  4. the longest gap between two lines after the block is at most BOUND seconds (the unit test TestDeadDirectPathFalls-
     BackToTheRelayInSeconds measures 6.5 s, bound 12 s), the stream goes on to the end with every number in order,
     and while the block lasts `pb status` names a relay path (wss or relay), not direct.
Prints one JSON line {"check": "dead-path-stream", ...}; exit 1 on failure.
Every run, pass or fail, saves what makes a slow cut provable (cut-16s.md, a 16.1 s gap with nothing but the gap saved):
`arrivals.csv` (t_ms from the block, line), `status.csv` (a 1 s sampler of selected path, relay region and home relay), and
the receipt names their directory (--trace-dir, default results/dead-path-<UTC>).
"""
import contextlib
import csv
import datetime
import json
import pathlib
import subprocess
import sys
import threading
import time

import path_display as pd
import winhost
from relay_home_wire import blocked
from pairing_lifecycle import command_result  # noqa: F401  (path_display's pb() uses it)


def selected(peer):
    return pd.machine_status(peer)['selected_path']


def status_sample(peer):
    """One `pb status` reading of the path to PEER and of this machine's home relay."""
    data = json.loads(pd.pb('status', '--json')[1])
    data = data.get('data', data)
    machine = next(m for m in data['machines'] if peer in (m['alias'], m['id']))
    home = data.get('home_relay') or {}
    return {'selected_path': machine.get('selected_path', ''), 'standby_path': machine.get('standby_path', ''),
            'relay_region': machine.get('relay_region', ''), 'home_region': home.get('region', '')}


def sample_status(peer, stop, samples, interval=1.0, sample=status_sample):
    """Append (time, reading) every INTERVAL seconds until STOP is set; a failed reading is kept as an error row."""
    while not stop.is_set():
        started = time.time()
        try:
            samples.append((started, sample(peer)))
        except Exception as err:  # noqa: BLE001
            samples.append((started, {'error': str(err)[:120]}))
        stop.wait(max(0.0, interval - (time.time() - started)))


def write_trace(directory, blocked_at, arrivals, samples):
    """Save the per-line arrivals and the status samples (t_ms from the block) and return what they show."""
    directory = pathlib.Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    ms = lambda t: round((t - blocked_at) * 1000)
    with open(directory / 'arrivals.csv', 'w', newline='') as out:
        writer = csv.writer(out)
        writer.writerow(['t_ms', 'line'])
        writer.writerows([ms(t), line.decode(errors='replace') if isinstance(line, bytes) else line] for t, line in arrivals)
    columns = ['selected_path', 'standby_path', 'relay_region', 'home_region', 'error']
    with open(directory / 'status.csv', 'w', newline='') as out:
        writer = csv.writer(out)
        writer.writerow(['t_ms'] + columns)
        writer.writerows([ms(t)] + [reading.get(column, '') for column in columns] for t, reading in samples)
    after = [ms(t) for t, _ in arrivals if t >= blocked_at]
    off_direct = [ms(t) for t, reading in samples if t >= blocked_at and reading.get('selected_path') not in ('direct', '', None)]
    homes = {reading['home_region'] for _, reading in samples if reading.get('home_region')}
    return {'dir': str(directory), 'first_line_after_ms': after[0] if after else None, 'first_non_direct_ms': off_direct[0] if off_direct else None,
            'home_region_changed': len(homes) > 1, 'status_errors': sum(1 for _, reading in samples if 'error' in reading)}


def stream_argv(peer, script, device=None):
    """The command that runs the stream client: pb here, or pb.exe on the Windows device behind its ssh login (stdout comes back line by line)."""
    if device is None:
        return [str(pd.PB), 'exec', peer, '--', 'sh', '-c', script]
    command = "& %s exec %s -- sh -c %s" % (winhost.ps_quote(device.pb_exe), winhost.ps_quote(peer), winhost.ps_quote(script))
    return device.ssh_prefix(device.target, 'powershell -NoProfile -NonInteractive -OutputFormat Text -EncodedCommand ' +
                             winhost._b64(command.encode('utf-16-le')))


@contextlib.contextmanager
def cut(address, device_addresses=()):
    """Silently drop the direct path for the duration; a Windows device is cut without restarting its pb.exe."""
    device = winhost.device()
    if not device:
        with (blocked(address) if address else pd.udp_blocked()):
            yield
    elif winhost.PEER_CUT:
        with winhost.peer_cut_held('silent', *device_addresses):
            yield
    else:
        raise RuntimeError('a Windows device needs SMOKE_PEER_CUT and --device-address: a cut on the device itself would restart its pb.exe')


def main():
    peer = sys.argv[1]
    trace_dir = sys.argv[sys.argv.index('--trace-dir') + 1] if '--trace-dir' in sys.argv else str(
        pathlib.Path(__file__).parent / 'results' / ('dead-path-' + datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')))
    bound = float(sys.argv[sys.argv.index('--bound') + 1]) if '--bound' in sys.argv else 12.0
    address = sys.argv[sys.argv.index('--peer-address') + 1] if '--peer-address' in sys.argv else None
    device_addresses = [sys.argv[i + 1] for i, word in enumerate(sys.argv) if word == '--device-address']
    for _ in range(45):  # a path takes seconds to settle after a start
        pd.pb('ping', peer)
        if selected(peer) == 'direct':
            break
        time.sleep(2)
    else:
        raise RuntimeError('no direct path to %s to kill (selected %s)' % (peer, selected(peer)))
    total = 900
    script = 'i=0; while [ $i -lt %d ]; do echo $i; i=$((i+1)); sleep 0.1; done' % total
    device = winhost.device()
    stream = subprocess.Popen(stream_argv(peer, script, device), stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                              stdin=subprocess.DEVNULL, bufsize=0, env=device.ssh_environment() if device else None)
    arrivals, samples, stop_sampling = [], [], threading.Event()
    threading.Thread(target=sample_status, args=(peer, stop_sampling, samples), daemon=True).start()

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
    with cut(address, device_addresses):
        blocked_at = time.time()
        deadline = blocked_at + 120
        while stream.poll() is None and time.time() < deadline and len(arrivals) < total:
            time.sleep(1)
            if time.time() - blocked_at > 40 and time.time() - arrivals[-1][0] < 3:
                break  # the stream is flowing again; give it a moment of steady flow, then release
        time.sleep(5)
        during = selected(peer)
    stop_sampling.set()
    trace = write_trace(trace_dir, blocked_at, arrivals, samples)
    after = [t for t, _ in arrivals if t >= blocked_at]
    if len(after) < 20:
        raise RuntimeError('the stream did not recover after the direct path to the peer was dropped: %d lines after the block (trace %s)' % (len(after), trace))
    gaps = [b - a for a, b in zip([blocked_at] + after, after)]
    longest = max(gaps)
    stream.terminate()
    numbers = [int(v) for _, v in arrivals if v.isdigit()]
    if numbers != list(range(len(numbers))):
        raise RuntimeError('the stream lost or repeated lines around the path change (trace %s)' % trace)
    if during == 'direct':
        raise RuntimeError('the stream flowed again but the path is still direct while the block lasts (trace %s)' % trace)
    if longest > bound:
        raise RuntimeError('the longest silence after the block was %.1f s (bound %.0f s; trace %s)' % (longest, bound, trace))
    print(json.dumps({'check': 'dead-path-stream', 'status': 'PASS', 'longest_gap_s': round(longest, 1), 'bound_s': bound,
                      'lines_before': len([t for t, _ in arrivals if t < blocked_at]), 'lines_after': len(after),
                      'path_while_blocked': during, 'in_order': True, 'trace': trace}))


if __name__ == '__main__':
    try:
        main()
    except Exception as err:  # noqa: BLE001
        print(json.dumps({'check': 'dead-path-stream', 'status': 'FAIL', 'detail': str(err)}))
        sys.exit(1)
