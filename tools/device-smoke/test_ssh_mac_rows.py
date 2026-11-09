import unittest
from unittest import mock

import ssh_mac_rows


# The Mac answers both ways a harness could pick the daemon: a name match also
# finds the user's own `pb daemon status` (888) and an old `pb daemon` (999);
# launchd names only its job's process (555).
def mac(script, timeout=120):
    if 'pgrep' in script:
        return '555 888 999\n'
    if 'launchctl print' in script:
        return 'gui/501/com.pinksaucepasta.paperboatd = {\n\tstate = running\n\tpid = 555\n}\n'
    return ''


class DaemonSelection(unittest.TestCase):
    def test_pauses_only_the_launchd_job_never_a_users_pb(self):
        with mock.patch.object(ssh_mac_rows, 'mac', mac):
            self.assertEqual(['555'], ssh_mac_rows.daemon_pids())

    def test_no_job_means_nothing_to_pause(self):
        with mock.patch.object(ssh_mac_rows, 'mac', lambda script, timeout=120: ''):
            self.assertEqual([], ssh_mac_rows.daemon_pids())


class OutageRevokeFirst(unittest.TestCase):
    def run_outage(self, env, mac_answer, client_answer):
        ssh_mac_rows.ARGS = mock.Mock(alias='apple', mac_user='admin')
        ssh_mac_rows.RESULTS.clear()
        with mock.patch.dict('os.environ', env, clear=True), \
                mock.patch.object(ssh_mac_rows, 'cl', client_answer), mock.patch.object(ssh_mac_rows, 'rcl', client_answer), \
                mock.patch.object(ssh_mac_rows, 'mac', mac_answer), mock.patch.object(ssh_mac_rows, 'msudo', lambda s, **k: ''):
            ssh_mac_rows.outage()
        return ssh_mac_rows.RESULTS[0]

    def test_without_a_throwaway_client_it_skips_instead_of_passing(self):
        self.assertEqual('SKIP', self.run_outage({}, lambda s, timeout=120: '', lambda s, timeout=300: '')['status'])

    def test_sshd_reading_another_file_fails(self):
        r = self.run_outage({'REVOKE_CLIENT_CMD': 'x'}, lambda s, timeout=120: '',
                            lambda s, timeout=300: 'sudo-ok warm-ok')
        self.assertEqual('FAIL', r['status'])
        self.assertIn('authorized_keys', r['detail'])


class RevokeVerdict(unittest.TestCase):
    ok = ('RESULT normal pb_stderr=[] plain_stderr=[]\nRESULT seconds=2.1 alive=no plain_seconds=2.3 plain_alive=no '
          'stderr=[pb: access to apple was revoked] plain_stderr=[pb: access to apple was revoked]')

    def test_both_sessions_ending_with_the_revoked_message_pass(self):
        self.assertTrue(ssh_mac_rows.revoke_verdict(self.ok)[0])

    def test_ssh_error_text_with_line_breaks_still_parses(self):
        self.assertTrue(ssh_mac_rows.revoke_verdict(self.ok.replace('stderr=[pb: access to apple was revoked]', 'stderr=[pb: access to apple was revoked\r\nclient_loop: Broken pipe\n pb: x]', 1))[0])

    def test_a_plain_session_that_stays_open_fails(self):
        self.assertFalse(ssh_mac_rows.revoke_verdict(self.ok.replace('plain_alive=no', 'plain_alive=yes'))[0])

    def test_a_missing_revoked_message_on_the_plain_session_fails(self):
        self.assertFalse(ssh_mac_rows.revoke_verdict(self.ok.replace('plain_stderr=[pb: access to apple was revoked]', 'plain_stderr=[]'))[0])

    def test_a_noisy_normal_exit_fails(self):
        self.assertFalse(ssh_mac_rows.revoke_verdict(self.ok.replace('pb_stderr=[]', 'pb_stderr=[oops]'))[0])


if __name__ == '__main__':
    unittest.main()
