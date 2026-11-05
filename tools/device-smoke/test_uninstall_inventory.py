"""Inventory scoping: other containers' processes are not this host's leftovers; a dangling pb link is one."""
from pathlib import Path
import tempfile
import unittest

from uninstall_inventory import product_process_count, retained


class UninstallInventory(unittest.TestCase):
    def test_only_processes_in_the_hosts_own_scope_count(self):
        listing = ['  101 pb', '  102 /usr/local/bin/pb', '  103 paperboat-hostd', '  104 bash', '  105 pb']
        host = {101, 103}  # 102 and 105 run in other containers
        self.assertEqual(product_process_count(listing, lambda pid: pid in host), 2)
        self.assertEqual(product_process_count(listing, lambda pid: True), 4)

    def test_this_hosts_container_processes_are_not_counted_through_the_namespace_check(self):
        # The real check reads /proc: the current process is in its own scope, a pid that does not exist is not.
        from uninstall_inventory import in_host_scope
        self.assertFalse(in_host_scope(2 ** 22 + 12345, linux=True))
        self.assertTrue(in_host_scope(1, linux=False))

    def test_a_dangling_pb_link_is_a_leftover(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            link = root / 'pb'
            link.symlink_to(root / 'removed-system-copy')
            self.assertFalse(link.exists())
            self.assertTrue(retained(link, root / 'inbox'))


if __name__ == '__main__':
    unittest.main()
