#!/usr/bin/env python3
"""F25-01..04 and C273..C278 on a Unix device: terminal recordings by hand and by rule, REC, list, play, export.

Usage: recordings_lifecycle.py TARGET --team TEAM [--vault-password-file FILE]
TARGET is a machine of TEAM that the signed-in account (a team owner or admin who may replay) reaches with pb connect.
Runs on a device with a Unix shell. Each phase prints one JSON line; the last line is the receipt. No terminal text,
password or recording content is printed: only counts, ids and the marker tokens this check generates itself.
"""
import argparse
import json
import os
import re
import select
import subprocess
import sys
import time
import uuid

import winhost
from pairing_lifecycle import command_result, invoke
from session_cleanup import cleanup, inventory
from terminal_lifecycle import Terminal

PB = winhost.pb_path()
REC_MARK = '\u25cf REC'.encode()  # the status bar's mark (statusbar_layout.go)
STARTED = 'recording'


def pb_json(*args, timeout=90):
    return invoke(PB, *args, '--json', timeout=timeout)


def wait_for(description, probe, timeout=60, interval=1.0):
    deadline = time.monotonic() + timeout
    while True:
        value = probe()
        if value:
            return value
        if time.monotonic() >= deadline:
            raise RuntimeError('timed out waiting for ' + description)
        time.sleep(interval)


def policy_set(path, team):
    result = command_result(PB, 'policy', 'set', str(path), '--workspace', team, '--yes', '--message', 'recordings device check', timeout=90)  # no --json for this command
    if result.returncode:
        raise RuntimeError('pb policy set failed: exit %d' % result.returncode)


def listing(team, machine=None):
    args = ['recordings', 'list', '--team', team]
    if machine:
        args += ['--machine', machine]
    return pb_json(*args)['recordings']


def unlock(password_file):
    if not password_file:
        return
    with open(password_file) as handle:
        result = command_result(PB, 'env', 'vault', 'unlock', '--password-stdin', '--json', input=handle.read(), timeout=90)
    if result.returncode:
        raise RuntimeError('the vault did not unlock; recordings cannot be replayed without it')


class Session:
    """One pb connect to the target, with the REC mark's presence read from its own output."""

    def __init__(self, target, name, status_bar='on', existing=False):
        self.target, self.name = target, name
        arguments = ['connect', target, '--session', name] if existing else ['connect', target, '--name', name]
        arguments += ['--workspace', os.environ['PB_SMOKE_WORKSPACE']]  # a team machine is not in the personal list
        # A fresh attach can lose its first connection attempts while the peer network settles after an install.
        for attempt in range(4):
            self.terminal = Terminal(winhost.terminal_argv(PB, [*arguments, '--status-bar', status_bar]), env=winhost.terminal_env(PB))
            try:
                self.terminal.expect(self.terminal.marker('READY', 'stty -echo; '), timeout=90)
                return
            except RuntimeError:
                self.terminal.close()
                if attempt == 3:
                    raise
                if not existing:
                    cleanup(PB, target, name)  # a half-made session would make the next attempt a name conflict
                time.sleep(15)

    def say(self, label, timeout=45):
        """A line only the shell's printf produces: the output reached this client."""
        token = self.terminal.marker(label)
        self.terminal.expect(token, timeout=timeout)
        return token

    def session_id(self):
        _, sessions = inventory(PB, self.target)
        owned = [s for s in sessions if s.get('name') == self.name]
        if len(owned) != 1:
            raise RuntimeError('the terminal session is missing or duplicated')
        return owned[0]['id']

    def saw_rec(self):
        self.terminal.settle(0.2)  # reads what the pty printed since the last look
        return REC_MARK in bytes(self.terminal.output)

    def end(self):
        try:
            self.terminal.settle()
            self.terminal.send(b'\x04')
            self.terminal.wait_exit()
        except RuntimeError:
            pass  # the terminal already closed: nothing left to end
        finally:
            self.terminal.close()


def recording_control(action, session_id):
    return pb_json('recordings', action, session_id)


def new_recording(team, target, before):
    return wait_for('the recording to be listed',
                    lambda: next((r for r in listing(team) if r['id'] not in before and r['machine_name'] == target), None), timeout=180, interval=3)


def finished(team, recording_id):
    return wait_for('the recording to end', lambda: next((r for r in listing(team) if r['id'] == recording_id and r['state'] == 'ended'), None), timeout=90)


def export_text(recording_id, directory):
    path = directory / ('export-' + uuid.uuid4().hex + '.cast')
    result = command_result(PB, 'recordings', 'export', recording_id, '--output', str(path), timeout=180)
    if result.returncode:
        raise RuntimeError('pb recordings export failed: exit %d: %s' % (result.returncode, (result.stderr or '')[-300:]))
    text = path.read_text()
    first = json.loads(text.splitlines()[0])
    if first.get('version') != 2:
        raise RuntimeError('the export is not asciicast v2')
    return text


def played_text(recording_id):
    result = command_result(PB, 'recordings', 'play', recording_id, '--idle-limit', '1s', '--speed', '8', timeout=180)
    if result.returncode:
        raise RuntimeError('pb recordings play failed: exit %d' % result.returncode)
    return result.stdout


def phase_manual(team, target, directory, replay=True):
    """F25-01 (CLI start and stop), F25-02 (REC in pb connect), F25-04 (list, play, export), C273-C278."""
    before = {r['id'] for r in listing(team)}
    name = 'smoke-rec-' + uuid.uuid4().hex[:10]
    session = Session(target, name)
    try:
        session.say('BEFORE')  # a recording starts with a snapshot of the screen, so earlier output may appear in it
        if session.saw_rec():
            raise RuntimeError('REC showed before anything was recorded')
        sid = session.session_id()
        started = recording_control('start', sid)
        if not started.get('recording') or started['session_id'] != sid:
            raise RuntimeError('recordings start did not report the recording')
        wait_for('REC in this client', session.saw_rec, timeout=30, interval=0.3)
        marker = session.say('ONAIR')
        recording = new_recording(team, target, before)
        if recording['trigger'] != 'manual' or recording['session_id'] != sid or recording['state'] != STARTED:
            raise RuntimeError('the listed recording is not this session\'s live manual recording')
        stopped = recording_control('stop', sid)
        if stopped.get('recording') is not None:
            raise RuntimeError('recordings stop did not clear the recording')
        after = session.say('AFTERSTOP')
        ended = finished(team, recording['id'])
        if ended['id'] != recording['id']:
            raise RuntimeError('another recording ended')
        if replay:
            text = export_text(recording['id'], directory)
            if marker not in text or '"o"' not in text:
                raise RuntimeError('the export does not hold the output recorded while REC was on')
            if after in text:
                raise RuntimeError('the recording holds output printed after it was stopped')
            if played_text(recording['id']).count(marker) < 1:
                raise RuntimeError('pb recordings play did not show the recorded output')
            by_machine = listing(team, ended['machine_id'])
            if recording['id'] not in {r['id'] for r in by_machine}:
                raise RuntimeError('--machine did not list the recording')
        return recording['id']
    finally:
        session.end()
        cleanup(PB, target, name)


def phase_long(team, target, directory):
    """F25-04: a long recording (about 8 MB of terminal output, many segments) streams to the end and verifies."""
    before = {r['id'] for r in listing(team)}
    name = 'smoke-rec-' + uuid.uuid4().hex[:10]
    session = Session(target, name)
    try:
        sid = session.session_id()
        recording_control('start', sid)
        wait_for('REC in this client', session.saw_rec, timeout=30, interval=0.3)
        done = 'PB_LONGDONE_' + uuid.uuid4().hex
        session.terminal.send("head -c 6000000 /dev/urandom | base64 | fold -w 120; printf 'PB_LONGDONE_%s\\n' " + done.split('_')[-1] + '\r')
        tail = b''
        deadline = time.monotonic() + 600
        while done.encode() not in tail:
            if time.monotonic() > deadline:
                raise RuntimeError('the long output did not finish')
            if select.select([session.terminal.fd], [], [], 1)[0]:
                chunk = os.read(session.terminal.fd, 1 << 16)  # drained, not kept: the recording is the evidence
                tail = (tail + chunk)[-256:]
                session.terminal.answer_queries(chunk)
        recording_control('stop', sid)
        recording = new_recording(team, target, before)
        ended = finished(team, recording['id'])
        if ended['segments'] < 4:
            raise RuntimeError('the long recording has only %d segments' % ended['segments'])
        text = export_text(recording['id'], directory)
        if text.count('\n') < 2 or len(text) < 6_000_000:
            raise RuntimeError('the export is shorter than the output it recorded (%d bytes)' % len(text))
        return recording['id']
    finally:
        session.end()
        cleanup(PB, target, name)


def phase_second_client(team, target):
    """F25-02: a client that attaches to a session being recorded shows REC without being told by anyone."""
    name = 'smoke-rec-' + uuid.uuid4().hex[:10]
    first = Session(target, name)
    second = None
    try:
        sid = first.session_id()
        recording_control('start', sid)
        wait_for('REC in the first client', first.saw_rec, timeout=30, interval=0.3)
        history = first.say('HIST')
        second = Session(target, name, existing=True)
        second.terminal.settle(0.5)
        seen = bytes(second.terminal.output)
        if REC_MARK not in seen:
            raise RuntimeError('a client attaching mid-recording does not show REC')
        if history.encode() in seen and seen.index(REC_MARK) > seen.index(history.encode()):
            raise RuntimeError('REC appeared only after the session\'s earlier output')
        recording_control('stop', sid)
    finally:
        for session in (second, first):
            if session is not None:
                session.end()
        cleanup(PB, target, name)


def phase_rule(team, target, directory, replay=True):
    """F25-03: a record rule starts a recording when a recorded account attaches; removing it stops every recording."""
    original = command_result(PB, 'policy', 'get', '--workspace', team, timeout=60)
    if original.returncode:
        raise RuntimeError('pb policy get failed')
    document = json.loads(original.stdout)
    document['rules'] = [*document['rules'], {'src': ['role:admin'], 'dst': ['machine:' + target], 'allow': ['record']}]
    path = directory / 'policy-rule.json'
    path.write_text(json.dumps(document))
    restore = directory / 'policy-original.json'
    restore.write_text(original.stdout)
    before = {r['id'] for r in listing(team)}
    name = 'smoke-rec-' + uuid.uuid4().hex[:10]
    session = None
    try:
        policy_set(path, team)
        session = Session(target, name)
        marker = session.say('RULEOUT')
        recording = new_recording(team, target, before)
        if recording['trigger'] != 'rule' or recording['state'] != STARTED:
            raise RuntimeError('the rule did not start the recording (trigger %s, state %s)' % (recording['trigger'], recording['state']))
        wait_for('REC in this client', session.saw_rec, timeout=30, interval=0.3)
        policy_set(restore, team)
        # The rule decides who is recorded from now on; a recording that already runs goes on until its session ends.
        other_name = 'smoke-rec-' + uuid.uuid4().hex[:10]
        known = {r['id'] for r in listing(team)}
        other = Session(target, other_name)
        try:
            other.say('NORULE')
            time.sleep(20)  # a start would be reported to the server within this
            if {r['id'] for r in listing(team) if r['machine_name'] == target} - known:
                raise RuntimeError('a session attached after the rule was removed was recorded')
        finally:
            other.end()
            cleanup(PB, target, other_name)
        session.say('STILLON')
        session.end()
        session = None
        ended = finished(team, recording['id'])
        if replay and marker not in export_text(ended['id'], directory):
            raise RuntimeError('the rule recording does not hold its output')
        return ended['id']
    finally:
        try:
            policy_set(restore, team)
        finally:
            if session is not None:
                session.end()
            cleanup(PB, target, name)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--team', required=True)
    parser.add_argument('--vault-password-file')
    parser.add_argument('--only', default='manual,second,rule,long')
    parser.add_argument('--no-replay', action='store_true', help='skip export and play: this account has no unlocked vault that opens the team recording key')
    args = parser.parse_args()
    os.environ['PB_SMOKE_WORKSPACE'] = args.team
    unlock(args.vault_password_file)
    if command_result(PB, 'recordings', '--help', timeout=60).returncode:
        raise RuntimeError('pb recordings --help failed')
    phases = args.only.split(',')
    started = time.monotonic()
    receipt = {'check': 'recordings', 'target': args.target, 'status': 'PASS', 'phases': []}
    with winhost.workdir('paperboat-recordings-') as directory:  # on the device under test
        if 'manual' in phases:
            receipt['manual_recording'] = phase_manual(args.team, args.target, directory, not args.no_replay)
            receipt['phases'] += ['manual_start_stop', 'rec_in_connect', 'cli_list'] + ([] if args.no_replay else ['cli_export', 'cli_play'])
        if 'long' in phases:
            receipt['long_recording'] = phase_long(args.team, args.target, directory)
            receipt['phases'].append('cli_export_long')
        if 'second' in phases:
            phase_second_client(args.team, args.target)
            receipt['phases'].append('rec_on_attach')
        if 'rule' in phases:
            receipt['rule_recording'] = phase_rule(args.team, args.target, directory, not args.no_replay)
            receipt['phases'] += ['rule_start', 'rule_removed_stops_new_recordings']
    receipt['seconds'] = round(time.monotonic() - started, 1)
    print(json.dumps(receipt), flush=True)


if __name__ == '__main__':
    main()
