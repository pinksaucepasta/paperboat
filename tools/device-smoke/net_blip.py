#!/usr/bin/env python3
"""F21-48: after a 5 min network blip the client recovers like the control plane: `pb doctor` stops reporting
healthy within 180 s of the cut, `pb exec` to TARGET fails while the network is down and works again within 30 s of
the network returning, and a native terminal attaches within the same 30 s.

  net_blip.py TARGET [--down-seconds 300]

The blip cuts the CLIENT: network_drop.cut() on Linux and macOS (see network_drop.py), or on a Windows device
(PB_SMOKE_WINDOWS_PB set) winhost's cut_client(), which blocks pb's own traffic there and leaves SSH alone (a cut of the
peer, SMOKE_PEER_CUT, would leave this client's doctor rightly healthy). It is lifted on every exit path and by a
failsafe after down-seconds + 180. A cut that can show what it blocked has to (report "cut"; all but a container's
iptables rule). Prints one JSON line.
"""
import argparse
import datetime
import json
from pathlib import Path
import subprocess
import sys
import time
import uuid
from urllib.parse import urlsplit

import network_drop
from session_cleanup import cleanup
from session_lifecycle import ready
from terminal_lifecycle import Terminal
import windows_target
import winhost

PB = network_drop.PB
RECOVER_SECONDS = 30
DOCTOR_DEADLINE = 180  # doctor must have stopped saying healthy by then (a Linux client: healthy at 90 s, unhealthy by ~170 s)
DOCTOR_PAUSE = 10
DOCTOR_ANSWER_SECONDS = 30  # a run that started by the deadline has this long after it to answer
LATE_SECONDS = 300  # only to tell slow from stuck in the report


def exec_ok(target, timeout=25):
    # a Windows target has no echo program; the platform is asked once, by the precondition, while the network is up
    shell = ['cmd', '/c'] if windows_target.is_windows(PB, target) else []
    try:
        return b'blip-ok' in winhost.run_pb(PB, ['exec', target, '--', *shell, 'echo', 'blip-ok'], timeout).stdout
    except subprocess.TimeoutExpired:  # expected while the network is down
        return False


def doctor_report():
    """What `pb doctor --json` reports: (overall, the checks that did not pass), ('', []) when it gave no report; doctor
    exits non-zero when it is not healthy."""
    try:
        out = winhost.run_pb(PB, ['doctor', '--json'], 120).stdout.decode(errors='replace')
        report = json.loads(out[out.index('{'):])
    except (subprocess.TimeoutExpired, ValueError):
        return '', []
    failing = ['%s=%s' % (c.get('code'), c.get('status')) for c in report.get('checks', []) if c.get('status') != 'pass']
    return report.get('overall', ''), failing


def doctor_flip(cut_at, deadline, expected=None):
    """Ask doctor again and again until it stops saying healthy (or says `expected`) or `deadline` seconds have passed
    since cut_at. A run only counts if it started by then and answered at most DOCTOR_ANSWER_SECONDS after. Returns (last
    overall, seconds it flipped at or None, samples)."""
    samples, overall = [], ''
    while time.monotonic() - cut_at <= deadline:
        overall, failing = doctor_report()
        at = round(time.monotonic() - cut_at, 1)
        samples.append([at, overall, failing])
        if at <= deadline + DOCTOR_ANSWER_SECONDS and (overall == expected if expected else overall not in ('', 'healthy')):
            return overall, at, samples
        time.sleep(DOCTOR_PAUSE)
    return overall, None, samples


def cut_client(seconds):
    """Cut the machine under test; returns (block, failsafe)."""
    if network_drop.DEVICE:
        return network_drop.DEVICE.cut_client(seconds), None
    return network_drop.cut(seconds)


def release_client(block):
    """Lifts the cut; returns what a Unix cut proves at its removal (the Windows cut's proof is read later)."""
    if network_drop.DEVICE:
        block.release()
        return None
    return network_drop.release(block)


def attach(target, name):
    """When a native terminal to target has its shell's prompt and answered a marker (RuntimeError if it never does)."""
    windows = windows_target.is_windows(PB, target)
    terminal = Terminal(winhost.terminal_argv(PB, ['connect', target, '--name', name, '--status-bar', 'off']), env=winhost.terminal_env(PB))
    try:
        ready(terminal, windows)
        return time.monotonic()
    finally:
        terminal.close()


def fence_events(since):
    device = network_drop.DEVICE
    directory = device.runtime_dir().parent / 'state' / 'diagnostics'
    # Diagnostics deliberately discard arbitrary log messages; the source
    # location is their retained evidence. Resolve it from this build's source.
    source = Path(__file__).resolve().parents[2] / 'cli/internal/peertransport/tailnet/regional_recovery.go'
    locations = {}
    for number, line in enumerate(source.read_text().splitlines(), 1):
        if 'diagnosticlog.TryWarn("every relay refused admission' in line:
            locations['entered'] = str(number)
        if 'diagnosticlog.TryInfo("relays accept this machine again' in line:
            locations['cleared'] = str(number)
    if len(locations) != 2:
        raise RuntimeError('cannot locate refusal diagnostics in the tested build source')
    lines = device.matching_lines(directory, '*.ndjson', 'regional_recovery.go')
    events = [json.loads(line) for line in lines]
    cutoff = datetime.datetime.fromisoformat(since.replace('Z', '+00:00'))
    fresh = [event for event in events
             if datetime.datetime.fromisoformat(event['at'].replace('Z', '+00:00')) >= cutoff]
    proof = [{'at': event['at'], 'source_file': source.name,
              'source_line': event['fields']['source_line'], 'severity': event['severity']}
             for event in fresh if event.get('fields', {}).get('source_line') in locations.values()]
    return {**{kind: sum(event['source_line'] == number for event in proof)
               for kind, number in locations.items()}, 'events': proof}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--down-seconds', type=int, default=300)
    parser.add_argument('--refusal-helper', help='Windows net-blip-refusal.exe, built from cli/tools/net-blip-refusal')
    parser.add_argument('--refusal-endpoint', help='exact throwaway CLI endpoint id to rotate and restore')
    args = parser.parse_args()
    forced = bool(args.refusal_helper)
    if bool(args.refusal_helper) != bool(args.refusal_endpoint) or forced and not network_drop.DEVICE:
        parser.error('forced refusal needs a Windows device, --refusal-helper and --refusal-endpoint')
    if not network_drop.DEVICE:
        network_drop.require_cut_tool()
    network_drop.join_cgroup()
    if not exec_ok(args.target):
        raise SystemExit('precondition: exec to %s does not work before the blip' % args.target)
    name = 'smoke-blip-' + uuid.uuid4().hex[:10]
    report = {}
    since = (network_drop.DEVICE.ps('device clock', '[DateTime]::UtcNow.ToString("o")')[1]
             if network_drop.DEVICE else datetime.datetime.now(datetime.timezone.utc).isoformat())
    control_block = cut_error = None
    block, failsafe = cut_client(args.down_seconds + 180)
    blocked = True
    try:
        cut_at = time.monotonic()
        if forced:
            device = network_drop.DEVICE
            server = json.loads((device.runtime_dir() / 'machine-registration.json').read_text())['server_url']
            addresses = json.loads(device.ps('resolve API on device',
                                   '[Net.Dns]::GetHostAddresses($req.host) | ForEach-Object {$_.IPAddressToString} | ConvertTo-Json -Compress',
                                   host=urlsplit(server).hostname)[1])
            if isinstance(addresses, str):
                addresses = [addresses]
            if not addresses:
                raise RuntimeError('the device could not resolve the API address')
            control_block = device.cut_network(args.down_seconds + 180, addresses=addresses)
            rotation = device.run([args.refusal_helper, '--endpoint', args.refusal_endpoint, '--server', server], timeout=90)
            if rotation.stdout.strip():
                report['refusal_rotation'] = json.loads(rotation.stdout)
            if rotation.returncode:
                raise RuntimeError('forced refusal registration failed: ' + rotation.stderr)
        report['doctor_while_down'], report['doctor_flipped_after_s'], report['doctor_samples'] = doctor_flip(
            cut_at, min(DOCTOR_DEADLINE, args.down_seconds), 'unhealthy' if forced else None)
        report['exec_while_down'] = exec_ok(args.target)
        time.sleep(max(0, args.down_seconds - (time.monotonic() - cut_at)))
        try:
            proof = release_client(block)
            if proof:
                report['cut'] = proof
        except (RuntimeError, subprocess.SubprocessError) as error:  # it did not hold: still measure the recovery, then fail
            report['cut'], cut_error = None, str(error)
        blocked = False
        if forced:
            # Keep only the API blocked: real relays now refuse the cached grants
            # below the floor advanced by the registration API. Never release the
            # API until a fresh diagnostic proves the refusal fence was entered.
            deadline = time.monotonic() + 60
            while True:
                events = fence_events(since)
                if events['entered']:
                    report['refused_fence_entries'] = events['entered']
                    break
                if time.monotonic() >= deadline:
                    raise RuntimeError('forced refusal never engaged the relay fence within 60 s')
                time.sleep(3)
            control_block.release()
            control_block = None
        restored = time.monotonic()
        report['exec_after_s'] = None
        while time.monotonic() - restored < RECOVER_SECONDS:
            if exec_ok(args.target, 10):  # short tries: one hung request must not use the whole 30 s
                report['exec_after_s'] = round(time.monotonic() - restored, 1)
                break
            time.sleep(1)
        if report['exec_after_s'] is None:  # the row fails; keep waiting so a slow recovery reads differently from a stuck one
            while time.monotonic() - restored < LATE_SECONDS and not exec_ok(args.target, 10):
                time.sleep(1)
            report['exec_late_s'] = round(time.monotonic() - restored, 1) if time.monotonic() - restored < LATE_SECONDS else None
        else:
            answered = attach(args.target, name)
            report['terminal_after_s'] = round(answered - restored, 1)
        if forced:
            events = fence_events(since)
            deadline = time.monotonic() + 15  # diagnostics persistence may trail the successful stream
            while not events['cleared'] and time.monotonic() < deadline:
                time.sleep(2)
                events = fence_events(since)
            report['refused_fence_clears'] = events['cleared']
            report['refusal_fence_events'] = events['events']
            verified = device.run([args.refusal_helper, '--endpoint', args.refusal_endpoint,
                                   '--server', server, '--verify'], timeout=45)
            if verified.returncode:
                raise RuntimeError('recovered registration verification failed: ' + verified.stderr)
            report['recovered_registration'] = json.loads(verified.stdout)
            if (report['recovered_registration']['key_generation'] !=
                    report['refusal_rotation']['key_generation_restored']):
                raise RuntimeError('recovered authority has not applied the restored registration')
    except (RuntimeError, subprocess.SubprocessError) as error:  # the terminal missed its deadline
        report['terminal_after_s'] = None
        report['error'] = str(error)
    finally:
        if blocked:
            try:
                release_client(block)
            except (RuntimeError, subprocess.SubprocessError) as error:
                print(error, file=sys.stderr, flush=True)
        if control_block is not None:
            control_block.release()
        if failsafe is not None and failsafe.poll() is None:
            failsafe.terminate()
        try:
            cleanup(PB, args.target, name)
        except Exception:
            pass
    if network_drop.DEVICE:  # read after the recovery timings: reading the capture and the event log takes a while
        try:
            report['cut'] = block.proof()
        except RuntimeError as error:
            report['cut'] = None
            report['error'] = ' '.join(filter(None, (report.get('error'), str(error))))
    if cut_error:
        report['error'] = ' '.join(filter(None, (report.get('error'), cut_error)))
    ok = (report.get('doctor_flipped_after_s') is not None and report.get('exec_while_down') is False and not cut_error
          and (not network_drop.DEVICE or bool(report.get('cut')))
          and report.get('exec_after_s') is not None and report['exec_after_s'] <= RECOVER_SECONDS
          and report.get('terminal_after_s') is not None and report['terminal_after_s'] <= RECOVER_SECONDS)
    if forced:
        ok = (ok and report.get('doctor_while_down') == 'unhealthy'
              and report.get('refused_fence_entries', 0) > 0 and report.get('refused_fence_clears', 0) > 0)
    print(json.dumps({'check': 'net-blip', 'target': args.target, 'down_seconds': args.down_seconds,
                      'started_at': since, 'forced_refusal': forced,
                      'status': 'PASS' if ok else 'FAIL', **report}), flush=True)
    return 0 if ok else 1


if __name__ == '__main__':
    sys.exit(main())
