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


class WindowsTargetScripts(unittest.TestCase):
    def test_the_member_scripts_use_the_target_commands_and_survive_crlf_and_case(self):
        import subprocess
        for script in (r.ACCESS, r.HOLD):
            self.assertIn('HOLD_PB=', script)  # the target's own hold/no-op commands come from the prologue
        self.assertIn('"$HOLD_PB"', r.HOLD)
        self.assertIn('"$HOLD_PLAIN"', r.HOLD)
        self.assertNotIn('echo session-up; sleep 300', r.HOLD.split('HOLD_PB')[-1])
        # a Windows sshd answers "ok\r\n" and the managed config names the machine in lower case
        probe = subprocess.run(['bash', '-c', 'A=W6-Win5-X; D=example.test; N=$(tr A-Z a-z <<<"$A.$D"); echo $N; '
                                'printf "ok\\r\\n" | tr -d "\\r" | tr "\\n" " " | grep -qE "(^| )ok $" && echo matched'],
                               capture_output=True, text=True)
        self.assertEqual(['w6-win5-x.example.test', 'matched'], probe.stdout.split())
        self.assertIn('N=$(tr A-Z a-z <<<"$A.$D")', r.ACCESS)
        # the Windows sshd listens on its own port: known_hosts names it '[host]:port', the Mac's names 'host'
        counted = subprocess.run(['bash', '-c', 'N=w6-x.ppr.boats; printf "[w6-x.ppr.boats]:44249 ssh-ed25519 K\\nw6-x.ppr.boats ssh-ed25519 K\\n'
                                  'other.ppr.boats ssh-ed25519 K\\n[w6-x.ppr.boats.evil]:1 ssh-ed25519 K\\n" | '
                                  + r.ACCESS.split('kh=$(')[1].split(' /run/user')[0] + ' | wc -l'], capture_output=True, text=True)
        self.assertEqual('2', counted.stdout.strip())
        self.assertIn("tr -d '\\r'", r.ACCESS)


if __name__ == '__main__':
    unittest.main()
