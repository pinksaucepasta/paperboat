"""Exercise service inventory against real systemctl filesystem enumeration."""
from pathlib import Path
import shutil
import tempfile
import unittest

from uninstall_inventory import systemd_service_count


@unittest.skipUnless(shutil.which('systemctl'), 'systemctl is Linux-specific')
class SystemdInventory(unittest.TestCase):
    def test_empty_and_owned_services_are_distinct_from_query_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            units = root / 'etc/systemd/system'
            units.mkdir(parents=True)
            unit = '[Unit]\nDescription=Inventory fixture\n[Service]\nExecStart=/bin/true\n'
            (units / 'unrelated.service').write_text(unit)
            command = ['systemctl', '--root', directory]
            self.assertEqual(systemd_service_count(command), 0)
            (units / 'paperboat-fixture.service').write_text(unit)
            self.assertEqual(systemd_service_count(command), 1)
            with self.assertRaises(RuntimeError):
                systemd_service_count(['systemctl', '--invalid-paperboat-inventory-option'])


if __name__ == '__main__':
    unittest.main()
