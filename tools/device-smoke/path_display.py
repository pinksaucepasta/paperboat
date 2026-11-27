#!/usr/bin/env python3
"""F21-06: pb status, pb doctor and the bug report show the selected path; a forced relay-only path is reported as such.

  path_display.py TARGET     (run as root-capable user on a paired Linux or macOS machine; needs sudo -n iptables or pfctl;
                             with PB_SMOKE_WINDOWS_PB the client is the Windows device and the block is a firewall rule)
  1. direct: after a ping, `pb status` names the direct path for TARGET, `pb doctor TARGET` peer_reachability says direct
  2. UDP blocked (iptables, always removed): pings go over the regional relay, `pb status` reports selected_path
     wss or relay with a relay_region, doctor says regional_relay, and the bug report's status.json carries both

From the moment UDP is blocked until just before it is unblocked, `pb status` is sampled and one wrong path in any
sample fails the check, however briefly it shows. selected_path is one of none, direct, relay, wss, mixed
(cli/internal/localapi/types.go transportSummary; localdaemon observations.go and snapshot_source.go set it):
  allowed  relay, wss  the forced path
           none        no path observed (yet): the state while pb is connecting; the relay must still show within 90 s
           mixed       only while every transport_consumers path is relay or wss
  wrong    direct, mixed with a direct consumer (or listing none), and any other value
standby_path is not the path in use: it is recorded and not judged. Every sample is in the receipt, with its time.
"""
import contextlib
import io
import json
import os
import re
import subprocess
import sys
import threading
import time
import zipfile

from pairing_lifecycle import command_result
import winhost

PB = winhost.pb_path()
DARWIN = os.uname().sysname == 'Darwin'
# The stock macOS pf.conf evaluates com.apple/* anchors, so no change to the main ruleset is needed.
PF_ANCHOR = 'com.apple/pbsmoke-path'
# silent: macOS pf drops these after sendto succeeded (5/5 sent, 0 on en0; rerun-silentcut-20261117.md)
PF_RULE = 'block drop out quick on ! lo0 proto udp from any to any port != 53\n'


def pb(*args):
    done = command_result(PB, *args, timeout=180)
    return done.returncode, done.stdout, done.stderr


@contextlib.contextmanager
def udp_blocked():
    device = winhost.device()
    if device:
        with device.udp_blocked():
            yield
        return
    if DARWIN:
        sudo = lambda *args, **kw: subprocess.run(['sudo', '-n', *args], capture_output=True, text=True, timeout=30, **kw)
        # A root failsafe drops the rule even if this process dies; the rule is UDP only, so the ssh/TCP session survives.
        failsafe = subprocess.Popen(['sudo', '-n', 'sh', '-c', 'sleep 600; /sbin/pfctl -a %s -F all' % PF_ANCHOR],
                                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
        try:
            if sudo('pfctl', '-a', PF_ANCHOR, '-f', '-', input=PF_RULE).returncode:
                raise RuntimeError('pfctl could not load the UDP drop rule into ' + PF_ANCHOR)
            token = re.search(r'Token : (\d+)', sudo('pfctl', '-E').stderr)
            sudo('pfctl', '-F', 'states')  # an established UDP flow keeps passing on its state until flushed
            try:
                yield
            finally:
                sudo('pfctl', '-a', PF_ANCHOR, '-F', 'all')
                if token:
                    sudo('pfctl', '-X', token.group(1))
        finally:
            failsafe.terminate()
        return
    # silent, as on a real dead path: an OUTPUT DROP would fail sendmsg with EPERM and pb would drop the path at once
    with winhost.linux_silent_cut(['!', '-o', 'lo', '-p', 'udp', '!', '--dport', '53'], ['!', '-i', 'lo', '-p', 'udp', '!', '--sport', '53']):
        yield


def bundle_status(path):
    """status.json of the bug report bundle, read where pb wrote it."""
    with zipfile.ZipFile(io.BytesIO(winhost.device_path(path).read_bytes())) as zipped:
        return zipped.read('status.json').decode()


def machine_status(target):
    data = json.loads(pb('status', '--json')[1])
    data = data.get('data', data)
    return next(m for m in data['machines'] if target in (m['alias'], m['id']))


def doctor_path(target):
    checks = json.loads(pb('doctor', target, '--json')[1])['checks']
    return next(c.get('selected_path') for c in checks if c['code'] == 'peer_reachability')


def wait_for(what, condition, seconds=90):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if condition():
            return
        time.sleep(2)
    raise RuntimeError('%s: not seen in %d s' % (what, seconds))


RELAY_PATHS = ('relay', 'wss')


def wrong_path(sample):
    """Whether one sample taken while the relay is forced reports a path that cannot be in use. A sample whose status
    could not be read reports none."""
    path = sample.get('path')
    if 'error' in sample or path == 'none' or path in RELAY_PATHS:
        return False
    if path == 'mixed':
        consumers = sample.get('consumers')
        return not consumers or any(consumer not in RELAY_PATHS for consumer in consumers)
    return True


class Sampler:
    """Reads the status over and over on its own thread, so nothing the check does leaves the window unwatched."""

    def __init__(self, read, pause=0.5):
        self.read, self.pause, self.samples = read, pause, []
        self.forced = time.monotonic()
        self.stopping = threading.Event()
        self.thread = threading.Thread(target=self._run, daemon=True)
        self.thread.start()

    def sample(self):
        asked = time.monotonic() - self.forced
        sample = {'t': round(asked, 2), 'at': time.strftime('%H:%M:%SZ', time.gmtime())}
        try:
            status = self.read()
            sample.update(path=status.get('selected_path'), region=status.get('relay_region', ''),
                          standby=status.get('standby_path', ''),
                          consumers=[c.get('path') for c in status.get('transport_consumers') or []])
        except Exception as error:  # recorded, never dropped: a status that cannot be read is part of the window
            sample['error'] = type(error).__name__
        sample['answered'] = round(time.monotonic() - self.forced, 2)
        self.samples.append(sample)

    def _run(self):
        while True:
            self.sample()
            if self.stopping.wait(self.pause):
                return

    def latest(self):
        return self.samples[-1] if self.samples else {}

    def stop(self):
        """One last sample after everything the check did, then no more: call it while the relay is still forced."""
        self.stopping.set()
        self.thread.join(timeout=400)  # past pb()'s own 180 s bound
        if self.thread.is_alive():
            raise RuntimeError('the pb status sampler did not stop')
        self.sample()


def window_failures(samples):
    failures = []
    if not any('error' not in sample for sample in samples):
        failures.append('pb status was never readable while the relay was forced')
    wrong = [sample for sample in samples if wrong_path(sample)]
    if wrong:
        # final-table.py annotates a known bug only from this exact text: which wrong states, and what the window ended on
        failures.append('pb status reported a wrong path in %d of %d samples while the relay was forced (wrong: %s; window ended on: %s; '
                        'first at %.2f s, last at %.2f s)' % (len(wrong), len(samples),
                                                              ','.join(sorted({str(s.get('path')) for s in wrong})),
                                                              samples[-1].get('path', 'unreadable'), wrong[0]['t'], wrong[-1]['t']))
    return failures


def main(receipt):
    target = sys.argv[1]
    ping = lambda: pb('ping', target)
    wait_for('direct', lambda: (ping(), machine_status(target)['selected_path'] == 'direct')[1])
    if doctor_path(target) != 'direct':
        raise RuntimeError('doctor does not say direct on a direct path')
    failures = []
    with udp_blocked():
        sampler = Sampler(lambda: machine_status(target))
        receipt['forced_at'] = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())
        receipt['samples'] = sampler.samples
        try:
            wait_for('relay path in status', lambda: (ping(), sampler.latest().get('path') in RELAY_PATHS)[1])
            status = machine_status(target)
            if status['selected_path'] not in RELAY_PATHS or not status.get('relay_region'):
                raise RuntimeError('status has no relay path with a relay_region: %s' % status['selected_path'])
            doctor = doctor_path(target)
            if doctor != 'regional_relay':
                raise RuntimeError('doctor says %s on a relay-only path' % doctor)
            bundle = json.loads(pb('bugreport', '--json')[1])
            report = bundle_status(bundle['bundle_path'])
            if status['selected_path'] not in report or status['relay_region'] not in report:
                raise RuntimeError('the bug report status.json lacks the selected path or relay region')
        except Exception as err:  # the window's samples are judged whatever else failed
            failures.append(str(err))
        finally:
            sampler.stop()
    failures += window_failures(sampler.samples)
    if failures:
        raise RuntimeError('; '.join(failures))
    receipt.update(forced_path=status['selected_path'], region=status['relay_region'])


if __name__ == '__main__':
    result = {'check': 'path-display', 'status': 'PASS'}
    try:
        main(result)
    except Exception as err:
        result.update(status='FAIL', detail=str(err))
    print(json.dumps(result))
    sys.exit(0 if result['status'] == 'PASS' else 1)
