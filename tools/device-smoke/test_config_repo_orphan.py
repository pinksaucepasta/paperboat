"""A config check that starts its repository server first stops the one an earlier killed run left, before it wipes the pid file
(two orphaned `python3 server.py` from 2026-11-13 kept session scopes alive on Linux test VM for a week)."""
import unittest
from unittest.mock import patch

import config_sync_pair
import xplat_config


class SeedStopsAnOrphanedServer(unittest.TestCase):
    def check(self, repo, root):
        scripts = []
        repo.sh = lambda script, *a, **k: scripts.append(script) or type('R', (), {'returncode': 0, 'stdout': b''})()
        repo.seed({})
        script = scripts[0]
        kill, wipe = script.find('kill $(cat %s/pid)' % root), script.find('rm -rf %s' % root)
        self.assertGreaterEqual(kill, 0, 'the seed never stops an earlier server')
        self.assertLess(kill, wipe, 'the pid file is wiped before the earlier server is stopped')

    def test_config_sync_pair(self):
        self.check(config_sync_pair.Repository.__new__(config_sync_pair.Repository), config_sync_pair.REPO_ROOT)

    def test_xplat_config(self):
        self.check(xplat_config.Repo(), xplat_config.REPO_ROOT)


if __name__ == '__main__':
    unittest.main()
