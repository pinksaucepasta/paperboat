import contextlib
import unittest
from pathlib import PureWindowsPath
from unittest import mock

import update_scheduled as u


class Transfers(unittest.TestCase):
    def test_a_windows_target_sends_a_file_that_is_on_the_device(self):
        device, written, sent = mock.Mock(), [], []

        @contextlib.contextmanager
        def workdir(prefix):
            yield PureWindowsPath('C:/Temp/updcheck-1')

        with mock.patch.object(u.winhost, 'workdir', workdir), mock.patch.object(u.winhost, 'device', lambda: device), \
                mock.patch.object(u.winhost, 'write_random', lambda path, size: written.append(str(path))), \
                mock.patch.object(u.p, 'invoke', lambda binary, *args, **kw: sent.append(args)), \
                mock.patch.object(u.p, 'command_result'):
            u.transfers('me', 'peer', 'during-activation')
        self.assertEqual(sent[1], ('send', written[0], '--to', 'peer'))
        self.assertIn('updcheck-during-activation.bin', device.ps.call_args.args[1])


if __name__ == '__main__':
    unittest.main()
