import json
import pathlib
import subprocess
import unittest
from unittest import mock

import recordings_key_rotation as krot

TEAM, TARGET = 'team_1', 'pool-1'


class Cli:
    """A stub pb: records every call in order and answers like the product (recordings_command.go)."""

    def __init__(self, reset_opens_old=False, fail_replace=False, status=True, first_generation=1):
        self.status, self.first_generation = status, first_generation
        self.calls, self.generation, self.reset_done, self.before_reset = [], 1, False, []
        self.reset_opens_old, self.fail_replace = reset_opens_old, fail_replace
        self.made = []

    def command_result(self, _binary, *args, **_):
        self.calls.append(args[:3] if args[:2] == ('recordings', 'key') else args[:2])  # keeps 'key --help' and 'key status' apart from replace/reset
        if args[:3] == ('recordings', 'key', '--help'):
            return subprocess.CompletedProcess(args, 0, 'Available Commands:\n  replace  r\n' + ('  status  s\n' if self.status else ''), '')
        if args[:3] == ('recordings', 'key', 'status'):
            return self.done(args, {'data': {'generation': self.first_generation if not self.made_after_replace() else self.generation}})
        if args[:3] == ('recordings', 'key', 'replace'):
            if self.fail_replace:
                return subprocess.CompletedProcess(args, 1, '', 'you hold no copy PB_SECRET_VALUE')
            self.generation += 1
            return self.done(args, {'team': TEAM, 'generation': self.generation, 'copies': 2})
        if args[:3] == ('recordings', 'key', 'reset'):
            self.generation, self.reset_done, self.before_reset = self.generation + 1, True, list(self.made)
            return self.done(args, {'team': TEAM, 'generation': self.generation, 'copies': 2, 'older_recordings_readable': False})
        if args[:2] in (('recordings', 'export'), ('recordings', 'play')):
            recording = args[2]
            gone = self.reset_done and recording in self.before_reset and not self.reset_opens_old
            return subprocess.CompletedProcess(args, 1 if gone else 0, '', 'cannot be opened' if gone else '')
        raise AssertionError(args)

    def made_after_replace(self):
        return self.generation > 1

    def done(self, args, receipt):
        return subprocess.CompletedProcess(args, 0, json.dumps(receipt), '')

    def phase_manual(self, *_):
        recording = 'rec_%d' % (len(self.made) + 1)
        self.calls.append(('phase_manual', recording))
        self.made.append(recording)
        return recording

    def listing(self, _team):
        return [{'id': r} for r in self.made]

    def played(self, recording):
        self.calls.append(('recordings', 'play'))
        if self.reset_done and recording in self.before_reset and not self.reset_opens_old:
            raise RuntimeError('play failed')


def run(cli):
    patches = [mock.patch.object(krot, 'command_result', cli.command_result),
               mock.patch.object(krot.rec, 'phase_manual', cli.phase_manual),
               mock.patch.object(krot.rec, 'listing', cli.listing),
               mock.patch.object(krot.rec, 'export_text', lambda r, d: cli.calls.append(('recordings', 'export')) or 'text'),
               mock.patch.object(krot.rec, 'played_text', lambda r: cli.played(r) or 'text')]
    for patch in patches:
        patch.start()
    try:
        return krot.run(TEAM, TARGET, pathlib.Path('/nonexistent'))
    finally:
        mock.patch.stopall()


class RotationTest(unittest.TestCase):
    def test_steps_run_in_order_and_assert_the_promises(self):
        cli = Cli()
        receipt = run(cli)
        self.assertEqual('PASS', receipt['status'])
        names = [s['step'] for s in receipt['steps']]
        self.assertEqual(['record_old', 'key_created_by_first_recording', 'replace', 'old_readable_after_replace', 'record_new', 'reset', 'old_unreadable_after_reset',
                          'new_unreadable_after_reset', 'listed_after_reset', 'record_after_reset'], names)
        kinds = [c[:2] if c[0] != 'recordings' or c[1] != 'key' else c for c in cli.calls]
        self.assertLess(kinds.index(('recordings', 'key', 'replace')), kinds.index(('recordings', 'key', 'reset')))
        self.assertEqual({'replace': 2, 'reset': 3}, receipt['generations'])  # +1 at each step
        self.assertEqual({'record_old': 'rec_1', 'record_new': 'rec_2', 'record_after_reset': 'rec_3'}, receipt['recordings'])

    def test_a_reset_that_leaves_old_recordings_open_fails(self):
        receipt = run(Cli(reset_opens_old=True))
        failed = {s['step'] for s in receipt['steps'] if s['status'] == 'FAIL'}
        self.assertEqual('FAIL', receipt['status'])
        self.assertEqual({'old_unreadable_after_reset', 'new_unreadable_after_reset'}, failed)
        self.assertEqual('PASS', receipt['steps'][-1]['status'])  # no fail-fast: the later steps still ran

    def test_a_failed_replace_skips_what_needs_it_but_not_the_rest(self):
        receipt = run(Cli(fail_replace=True))
        status = {s['step']: s['status'] for s in receipt['steps']}
        self.assertEqual('PASS', status['record_old'])
        self.assertEqual('FAIL', status['replace'])
        self.assertTrue(all(status[n] == 'SKIP' for n in ('old_readable_after_replace', 'record_new', 'reset', 'record_after_reset')))
        self.assertEqual('FAIL', receipt['status'])

    def test_no_secret_reaches_the_receipt(self):
        text = json.dumps(run(Cli(fail_replace=True)))
        self.assertNotIn('PB_SECRET_VALUE', text)  # a failed command's stderr is never copied into the receipt
        self.assertIn('exit 1', text)

    def test_a_failed_first_recording_is_reported_and_the_chain_steps_are_skipped(self):
        cli = Cli()
        cli.phase_manual = mock.Mock(side_effect=RuntimeError('REC showed before anything was recorded'))
        receipt = run(cli)
        self.assertEqual('FAIL', receipt['status'])
        self.assertEqual('FAIL', receipt['steps'][0]['status'])
        self.assertNotIn(('recordings', 'key', 'replace'), cli.calls)  # nothing was rotated without an old recording to check

    def test_a_first_recording_that_made_no_key_is_a_loud_fail(self):
        receipt = run(Cli(first_generation=0))
        failed = {s['step']: s['error'] for s in receipt['steps'] if s['status'] == 'FAIL'}
        self.assertEqual("the team's first recording did not create recording key generation 1", failed['key_created_by_first_recording'])
        self.assertEqual('FAIL', receipt['status'])

    def test_a_cli_that_cannot_show_the_generation_is_a_loud_fail_never_a_skip(self):
        receipt = run(Cli(status=False))
        step = next(s for s in receipt['steps'] if s['step'] == 'key_created_by_first_recording')
        self.assertEqual('FAIL', step['status'])
        self.assertIn('cannot read recording key generation', step['error'])
        self.assertNotIn('SKIP', [s['status'] for s in receipt['steps'] if s['step'] == 'key_created_by_first_recording'])
