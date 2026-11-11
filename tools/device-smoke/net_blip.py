#!/usr/bin/env python3
"""F21-48: after a 5 min network blip the client recovers like the control plane: `pb doctor` does not report
healthy while the network is down, `pb exec` to TARGET works again within 30 s of the network returning, and a
native terminal attaches within the same 30 s.

  net_blip.py TARGET [--down-seconds 300]

The blip is network_drop.cut(): iptables owner-drop (Linux), a pf anchor (macOS) or a firewall block of pb.exe's own
traffic (a Windows device, PB_SMOKE_WINDOWS_PB set), lifted on every exit path and by a failsafe after
down-seconds + 60. Prints one JSON line.
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
from terminal_lifecycle import Terminal
import winhost

PB = network_drop.PB
RECOVER_SECONDS = 30
LATE_SECONDS = 300  # only to tell slow from stuck in the report


def exec_ok(target, timeout=25):
    try:
        return b'blip-ok' in winhost.run_pb(PB, ['exec', target, '--', 'echo', 'blip-ok'], timeout).stdout
    except subprocess.TimeoutExpired:  # expected while the network is down
        return False


def doctor_overall():
    """The overall state `pb doctor --json` reports; doctor exits non-zero when it is not healthy."""
    out = winhost.run_pb(PB, ['doctor', '--json'], 120).stdout.decode(errors='replace')
    return json.loads(out[out.index('{'):]).get('overall', '')


def attach(target, name, deadline):
    """A native terminal to target answers a marker before the monotonic deadline (RuntimeError otherwise)."""
    start = time.monotonic()
    terminal = Terminal(winhost.terminal_argv(PB, ['connect', target, '--name', name, '--status-bar', 'off']), env=winhost.terminal_env(PB))
    try:
        terminal.expect(terminal.marker('BLIP', 'stty -echo; '), timeout=max(1, deadline - start))
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
    if not network_drop.DEVICE and network_drop.sudo(*(['pfctl', '-s', 'info'] if network_drop.DARWIN else ['iptables', '-S', 'OUTPUT'])).returncode:
        raise RuntimeError('needs sudo without a prompt for ' + ('pfctl' if network_drop.DARWIN else 'iptables'))
    if not exec_ok(args.target):
        raise SystemExit('precondition: exec to %s does not work before the blip' % args.target)
    name = 'smoke-blip-' + uuid.uuid4().hex[:10]
    report = {}
    since = (network_drop.DEVICE.ps('device clock', '[DateTime]::UtcNow.ToString("o")')[1]
             if network_drop.DEVICE else datetime.datetime.now(datetime.timezone.utc).isoformat())
    control_block = None
    block, failsafe = network_drop.cut(args.down_seconds + 180)
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
        time.sleep(min(90, args.down_seconds // 2))
        report['doctor_while_down'] = doctor_overall()
        report['exec_while_down'] = exec_ok(args.target)
        time.sleep(max(0, args.down_seconds - (time.monotonic() - cut_at)))
        network_drop.release(block)
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
            answered = attach(args.target, name, restored + RECOVER_SECONDS)
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
    except RuntimeError as error:  # the terminal missed its deadline
        report['terminal_after_s'] = None
        report['error'] = str(error)
    finally:
        if blocked:
            network_drop.release(block)
        if control_block is not None:
            control_block.release()
        if failsafe is not None and failsafe.poll() is None:
            failsafe.terminate()
        try:
            cleanup(PB, args.target, name)
        except Exception:
            pass
    ok = (report.get('doctor_while_down') not in (None, '', 'healthy') and report.get('exec_while_down') is False
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
