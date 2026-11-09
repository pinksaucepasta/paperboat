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


if __name__ == '__main__':
    unittest.main()
