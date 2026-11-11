import unittest

import pairing_lifecycle
import windows_lifecycle

WARNING = 'daemon: warning: The local daemon is ready. ' + pairing_lifecycle.HTTP_SYS_PORT_WARNING + '.'


class DoctorOkTest(unittest.TestCase):
    def test_healthy(self):
        self.assertTrue(windows_lifecycle.doctor_ok('doctor: healthy'))

    def test_only_the_runner_http_sys_warning_is_tolerated(self):
        self.assertTrue(windows_lifecycle.doctor_ok('doctor: degraded\n' + WARNING))
        self.assertFalse(windows_lifecycle.doctor_ok('doctor: degraded\ndaemon: warning: another program owns port 80'))
        self.assertFalse(windows_lifecycle.doctor_ok('doctor: degraded\nudp_ipv4: fail: blocked\n' + WARNING))
        self.assertFalse(windows_lifecycle.doctor_ok('doctor: unhealthy\n' + WARNING))


if __name__ == '__main__':
    unittest.main()
