import io
import json
import os
import unittest
from contextlib import redirect_stdout
from unittest import mock

os.environ.setdefault('TEAM', 'team-x')
import ssh_mac_member_revoke as r  # noqa: E402

HELD = ('RESULT normal pb_stderr=[] plain_stderr=[]\nRESULT exit10 fails=0 seconds=[ 1.0 ] bad=[ ]\nHOLDING\nRESULT seconds=2.0 alive=no plain_seconds=2.1 plain_alive=no '
        'stderr=[pb: access to apple was revoked] plain_stderr=[pb: access to apple was revoked]')
ACCESS = 'RESULT access plain_s=1.2 pb_s=1.4 config_block=1 known_hosts_lines=3'


def run_main(mode, access=ACCESS):
    owner_calls = []

    def member(script, timeout=120):
        if 'pb login' in script:
            return 0, ''
        if 'login.out' in script:
            return 0, 'open https://example.test/authorize?code=ABCD-1234'
        if 'auth status' in script:
            return 0, json.dumps({'account': {'id': 'user_b', 'email': 'b@example.test'}})
        if 'RESULT access' in script or 'sleep 1; done; N=' in script or 't0=$(date' in script:
            return 0, access
        if 'cat ~/hold.out' in script or 'tail -c' in script:
            return 0, HELD
        return 0, ''

    def owner(script, timeout=120):
        owner_calls.append(script)
        return 0, '{"invitation_id": "inv_1"}' if 'team invite' in script else ''

    out = io.StringIO()
    with mock.patch.object(r, 'MODE', mode), mock.patch.object(r, 'owner', owner), mock.patch.object(r, 'member', member), \
            mock.patch.object(r.subprocess, 'run', return_value=mock.Mock(returncode=0)), mock.patch.dict(os.environ, {'MEMBER_STATE': 'x'}), \
            mock.patch.object(r.time, 'sleep', lambda s: None), mock.patch.object(r.sys, 'argv', ['x', 'machine_t']), redirect_stdout(out):
        with mock.patch("sys.exit") as exit_:
            r.main()
    return json.loads(out.getvalue().splitlines()[-1]), owner_calls, exit_


class MemberRevoke(unittest.TestCase):
    def test_grant_mode_withdraws_the_grant_and_undoes_the_share_and_membership(self):
        result, calls, _ = run_main('grant')
        self.assertEqual('PASS', result['status'], result)
        self.assertTrue(any('--active=false' in c for c in calls))
        self.assertTrue(any('machine unshare' in c and 'team remove' in c for c in calls))

    def test_team_mode_removes_the_member_and_does_not_remove_twice(self):
        result, calls, _ = run_main('team')
        self.assertEqual('PASS', result['status'], result)
        self.assertEqual(1, sum('team remove "$TEAM" \'user_b\'' in c or "team remove \"$TEAM\" user_b" in c for c in calls[:-1]))

    def test_a_noisy_plain_exit_fails_the_check(self):
        global HELD
        saved = HELD
        HELD = saved.replace('fails=0', 'fails=1')
        try:
            result, _, _ = run_main('grant')
        finally:
            HELD = saved
        self.assertEqual('FAIL', result['status'])

    def test_a_slow_member_gate_fails_before_any_revoke(self):
        result, calls, _ = run_main('grant', 'RESULT access plain_s=31.0 pb_s=1.4 config_block=1 known_hosts_lines=3')
        self.assertEqual('FAIL', result['status'])
        self.assertFalse(any('--active=false' in c for c in calls))


if __name__ == '__main__':
    unittest.main()
