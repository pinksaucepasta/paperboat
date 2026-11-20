"""stage_binary streams a build gzipped once per host and copies host-local after that (scp's SFTP crawled on the VM link)."""
import os
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent


class StageBinary(unittest.TestCase):
    def test_first_staging_streams_and_caches_then_a_second_copies_host_local(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            log = tmp / 'ssh.log'
            shim = tmp / 'bin'
            shim.mkdir()
            # ssh HOST CMD: HOST is ignored, CMD runs here with this process's stdin (a pipe stands in for the VM)
            (shim / 'ssh').write_text('#!/bin/bash\nwhile [ "${1:0:1}" = - ]; do shift; done\nshift\necho "$1" >> %s\nexec bash -c "$1"\n' % log)
            (shim / 'ssh').chmod(0o755)
            build = tmp / 'pb'
            build.write_bytes(os.urandom(1) * 100_000 + os.urandom(2000))  # compressible, unique per run
            env = dict(os.environ, PATH='%s:%s' % (shim, os.environ['PATH']))
            first, second = tmp / 'first', tmp / 'second'
            script = 'set -e; . %s/stage_binary.sh; stage_binary %s fakehost %s; stage_binary %s fakehost %s' % (HERE, build, first, build, second)
            try:
                subprocess.run(['bash', '-c', script], check=True, env=env, timeout=60)
                self.assertEqual(first.read_bytes(), build.read_bytes())
                self.assertEqual(second.read_bytes(), build.read_bytes())
                self.assertTrue(stat.S_IMODE(first.stat().st_mode) & 0o111, 'a staged build must be executable')
                calls = log.read_text().splitlines()
                streams = [c for c in calls if 'gunzip' in c]
                self.assertEqual(len(streams), 1, 'the build crossed the link %d times: %s' % (len(streams), calls))
            finally:
                subprocess.run('rm -f /tmp/.pb-stage-$(sha256sum %s | cut -c1-24)*' % build, shell=True)  # the shim kept the cache on this machine's /tmp

    def test_local_host_is_a_plain_copy(self):
        with tempfile.TemporaryDirectory() as tmp:
            build, dest = Path(tmp) / 'pb', Path(tmp) / 'out'
            build.write_bytes(b'x' * 1000)
            subprocess.run(['bash', '-c', '. %s/stage_binary.sh; stage_binary %s local %s' % (HERE, build, dest)], check=True, timeout=30)
            self.assertEqual(dest.read_bytes(), build.read_bytes())
            self.assertTrue(stat.S_IMODE(dest.stat().st_mode) & 0o111)


if __name__ == '__main__':
    unittest.main()
