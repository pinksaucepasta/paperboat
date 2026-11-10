import base64
import subprocess
import unittest
from unittest import mock

import ssh_mac_rows
import ssh_windows_target


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


class FakeDevice:
    """Records what the Windows target would run on the device."""

    def __init__(self):
        self.calls = []

    def start(self, argv, directory):
        self.calls.append(('later', base64.b64decode(argv[-1]).decode('utf-16-le')))

    def temp_dir(self, prefix):
        return mock.MagicMock()

    def suspend(self, pid):
        self.calls.append(('suspend', pid))

    def resume(self, pid):
        self.calls.append(('resume', pid))

    def ps(self, step, script, **payload):
        self.calls.append((step, payload))
        return 0, ''

    def services(self, pattern):
        return [{'Name': pattern, 'Status': 'Running'}]


def windows_target(keys=('C:\\ssh\\authorized_keys\\paperboat',)):
    target = ssh_windows_target.Target.__new__(ssh_windows_target.Target)
    target.device = FakeDevice()
    target.sshd = {'service': 'PaperboatSshd-u1-g1', 'port': 40001, 'authorized_keys': list(keys), 'host_key': 'C:\\ssh\\hostkeys\\k'}
    return target


class WindowsTarget(unittest.TestCase):
    def setUp(self):
        ssh_mac_rows.ARGS = mock.Mock(alias='winbox', mac_user='runneradmin')
        ssh_mac_rows.RESULTS.clear()
        self.target = windows_target()
        patch = mock.patch.object(ssh_mac_rows, 'WIN', self.target)
        patch.start()
        self.addCleanup(patch.stop)

    def test_every_client_script_is_valid_bash_for_both_targets(self):
        scripts = (ssh_mac_rows.REVOKE, ssh_mac_rows.FORWARDING, 'ssh $H $NOP')
        for win in (self.target, None):
            with mock.patch.object(ssh_mac_rows, 'WIN', win):
                for script in scripts:
                    done = subprocess.run(['bash', '-n'], input=ssh_mac_rows.prelude() + script, capture_output=True, text=True)
                    self.assertEqual('', done.stderr)

    def test_the_mac_target_keeps_its_commands(self):
        with mock.patch.object(ssh_mac_rows, 'WIN', None):
            prelude = ssh_mac_rows.prelude()
        self.assertIn("NOP=true; HOLD_PB='echo session-up; sleep 300'; ", prelude)
        self.assertIn("SSHD_PORT=22; AGENT_LIST='ssh-add -l'; SHOW_DISPLAY='echo \"display=<$DISPLAY>\"'; ", prelude)

    def test_the_windows_commands_run_in_powershell_and_forward_to_the_managed_port(self):
        remote = self.target.remote()
        script = base64.b64decode(remote['HOLD_PB'].split()[-1]).decode('utf-16-le')
        self.assertTrue(script.endswith("'session-up';Start-Sleep 300"))
        self.assertEqual(('exit 0', '40001'), (remote['NOP'], remote['SSHD_PORT']))
        self.assertIn('H=runneradmin@$HN', ssh_mac_rows.prelude())

    def test_refusals_stop_the_managed_sshd_and_pause_the_services_with_a_timed_undo(self):
        answers = {'sshd_stopped': "pb: the machine's SSH server closed the connection", 'machine_offline': 'pb: Paperboat could not reach winbox',
                   'service_stopped': 'pb: this computer\u2019s Paperboat service is not running'}

        def client(script, timeout=300):
            for name, text in answers.items():
                if 'CASE ' + name in script:
                    return 'CASE %s [%s]\n' % (name, text)
            return 'warm-ok rc=0'

        with mock.patch.object(ssh_mac_rows, 'cl', client), mock.patch.object(ssh_mac_rows.time, 'sleep', lambda s: None), \
                mock.patch.object(self.target, 'daemon_pids', lambda: ['11', '12']):
            ssh_mac_rows.refusals()
        self.assertEqual('PASS', ssh_mac_rows.RESULTS[-1]['status'], ssh_mac_rows.RESULTS)
        calls = self.target.device.calls
        self.assertEqual("Start-Sleep 0;Stop-Service -Name 'PaperboatSshd-u1-g1' -Force;Start-Sleep 40;Start-Service -Name 'PaperboatSshd-u1-g1'", calls[0][1])
        self.assertTrue(calls[1][1].startswith('Start-Sleep 50;') and 'NtResumeProcess' in calls[1][1] and '11,12' in calls[1][1])
        self.assertEqual([('suspend', '11'), ('suspend', '12'), ('resume', '11'), ('resume', '12')], calls[2:])

    def test_the_block_is_removed_on_the_machine_by_a_timer_set_before_it(self):
        name = self.target.block(['192.0.2.1', '2001:db8::1'])
        (_, later), (_, payload) = self.target.device.calls
        self.assertEqual('Start-Sleep 240;Remove-NetFirewallRule -DisplayName ' + name, later)
        self.assertEqual({'name': name, 'addresses': ['192.0.2.1', '2001:db8::1']}, payload)

    def test_an_sshd_reading_a_second_key_file_fails_the_outage_check(self):
        self.target.sshd['authorized_keys'].append('.ssh/authorized_keys')
        answer = lambda s, timeout=300: 'sudo-ok warm-ok'
        with mock.patch.dict('os.environ', {'REVOKE_CLIENT_CMD': 'x'}), mock.patch.object(ssh_mac_rows, 'cl', answer), \
                mock.patch.object(ssh_mac_rows, 'rcl', answer):
            ssh_mac_rows.outage()
        self.assertEqual('FAIL', ssh_mac_rows.RESULTS[-1]['status'])
        self.assertIn('exactly one authorized keys file', ssh_mac_rows.RESULTS[-1]['detail'])

    def test_otheruser_cannot_pass_without_creating_its_key(self):
        with mock.patch.object(ssh_mac_rows, 'cl', lambda s, timeout=300: 'warm-ok'):
            ssh_mac_rows.otheruser()
        self.assertEqual('FAIL', ssh_mac_rows.RESULTS[-1]['status'])

    def test_otheruser_requires_the_offered_managed_key_to_be_refused(self):
        good = ('RESULT ok:[ setup-user-pb setup-user-plain bob-key-plain bob-agent-pb bob-agent-plain '
                'bob-password-plain bob-password-pb bob-wrong-password-is-sshd ] fail:[ ] skip:[ ]')
        for verdict, expected in [('refused', 'PASS'), ('not-refused', 'FAIL')]:
            with self.subTest(verdict=verdict):
                ssh_mac_rows.RESULTS.clear()

                def client(script, timeout=300, input=None):
                    if 'ssh-keygen' in script:
                        return 'ssh-ed25519 synthetic-fixture'
                    if 'MANAGED_OTHER_USER=' in script:
                        return 'MANAGED_OTHER_USER=' + verdict
                    if 'check setup-user-pb' in script:
                        return good
                    return 'warm-ok'

                with mock.patch.object(ssh_mac_rows, 'cl', client), \
                        mock.patch.object(self.target, 'create_other_user', return_value='S-1-5-21-test'), \
                        mock.patch.object(self.target, 'remove_other_user') as remove:
                    ssh_mac_rows.otheruser()
                self.assertEqual(expected, ssh_mac_rows.RESULTS[-1]['status'])
                remove.assert_called_once_with('S-1-5-21-test')


if __name__ == '__main__':
    unittest.main()
