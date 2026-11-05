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


if __name__ == '__main__':
    unittest.main()
