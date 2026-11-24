"""Pool runners end with their machine records revoked, by exact id, before the runner stops (mocks only)."""
import json
import os
import pathlib
import subprocess
import tempfile
import unittest

HERE = pathlib.Path(__file__).resolve().parent
ID1 = 'machine_11111111-1111-1111-1111-111111111111'
ID2 = 'machine_22222222-2222-2222-2222-222222222222'
REC = 'machine_33333333-3333-3333-3333-333333333333'
# ssh stub: the jump host answers `ss -ltnH` from STUB_UP; every call is logged; stdin (SQL) is kept.
SSH = '''#!/bin/bash
echo "ssh $*" >> "$STUB_LOG"
case "$*" in
  *"ss -ltnH"*) printf '%s\\n' "$STUB_UP"; exit 0 ;;
  *docker*psql*) cat > "$STUB_SQL"; exit 0 ;;
esac
cat > /dev/null 2>&1 || true
[ -z "${STUB_SSH_FAIL:-}" ]
'''
# gh stub: STUB_QUEUE_REPO is the repository (GH_REPO) with a queued macpool run whose job is STUB_QUEUE_JOB.
GH = '''#!/bin/bash
echo "gh $*" >> "$STUB_LOG"
[ -n "${STUB_QUEUE_REPO:-}" ] && [ "${GH_REPO:-}" = "$STUB_QUEUE_REPO" ] || exit 0
case "$*" in
  "run list"*queued*) echo 77 ;;
  *actions/runs/77/jobs*) echo "$STUB_QUEUE_JOB" ;;
esac
'''
# cleanup stub: a dry run lists two ids, --apply reports them; STUB_CLEANUP_FAIL fails every call.
CLEANUP = '''#!/bin/bash
echo "cleanup $*" >> "$STUB_LOG"
[ -z "${STUB_CLEANUP_FAIL:-}" ] || { echo boom >&2; exit 1; }
case "$*" in
  *--apply*) echo "revoked 3 machine(s): ${STUB_IDS}" ;;
  *) echo "would revoke 2 machine(s): %s %s" ;;
esac
''' % (ID1, ID2)


class Env:
    def __init__(self, test):
        self.dir = tempfile.TemporaryDirectory()
        test.addCleanup(self.dir.cleanup)
        root = pathlib.Path(self.dir.name)
        self.home, self.bin = root / 'home', root / 'bin'
        (self.home / '.ssh').mkdir(parents=True)
        self.bin.mkdir()
        (self.home / '.ssh' / 'winpool_controller').write_text('k')
        for name, body in (('ssh', SSH), ('gh', GH), ('cleanup', CLEANUP)):
            (self.bin / name).write_text(body)
            (self.bin / name).chmod(0o755)
        self.log, self.sql = root / 'log', root / 'sql'
        self.log.touch()

    def run(self, pool, *args, up='', **extra):
        env = dict(os.environ, HOME=str(self.home), PATH=f'{self.bin}:{os.environ["PATH"]}', WINPOOL_JUMP='jump',
                   STUB_LOG=str(self.log), STUB_SQL=str(self.sql), STUB_UP=up, STUB_IDS=f'{ID1} {ID2} {REC}',
                   POOL_REVOKE_CMD=str(self.bin / 'cleanup'), **{pool.upper() + '_OWNER': '/work/a'}, **extra)
        return subprocess.run(['bash', str(HERE / f'{pool}.sh'), *args], env=env, capture_output=True, text=True, timeout=60)

    def claim(self, pool, n, owner='/work/a', age_min=60):
        claims = self.home / '.ssh' / f'{pool}-claims'
        claims.mkdir(exist_ok=True)
        (claims / str(n)).write_text(owner + '\n')
        old = os.stat(claims / str(n)).st_mtime - age_min * 60
        os.utime(claims / str(n), (old, old))
        return claims

    def calls(self):
        return self.log.read_text().splitlines()


class PoolRevokeTest(unittest.TestCase):
    def setUp(self):
        self.env = Env(self)

    def test_stop_revokes_exact_ids_before_the_runner_stops(self):
        for pool, port in (('linuxpool', 22403), ('macpool', 22303), ('winpool', 22203)):
            with self.subTest(pool=pool):
                self.env.log.write_text('')
                claims = self.env.claim(pool, 3)
                done = self.env.run(pool, 'stop', '3', up=f'127.0.0.1:{port} ')
                self.assertEqual(done.returncode, 0, done.stderr)
                calls = [c for c in self.env.calls() if c.startswith('cleanup') or f' {pool}-3 ' in c]
                self.assertEqual(len(calls), 3, calls)
                self.assertEqual(calls[0], f'cleanup --pool {pool} --slot 3 --all --stale 0 seconds')
                self.assertIn(f'--id {ID1} --id {ID2}', calls[1])
                self.assertTrue(calls[1].startswith(f'cleanup --pool {pool} --slot 3 --id ') and calls[1].endswith('--apply'))
                self.assertTrue(calls[2].startswith(f'ssh -o BatchMode=yes -o ConnectTimeout=15 {pool}-3 '), calls[2])
                self.assertFalse((claims / '3').exists())

    def test_recorded_id_is_revoked_even_if_the_server_lookup_misses_it(self):
        claims = self.env.claim('linuxpool', 5)
        done = self.env.run('linuxpool', 'record', '5', REC)
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual((claims / '5.machines').read_text().split(), [REC])
        bad = self.env.run('linuxpool', 'record', '5', 'machine_x; rm -rf /')
        self.assertNotEqual(bad.returncode, 0)
        self.env.run('linuxpool', 'stop', '5', up='127.0.0.1:22405 ')
        apply = [c for c in self.env.calls() if c.endswith('--apply')]
        self.assertEqual(len(apply), 1)
        self.assertEqual(apply[0].count('--id'), 3)  # recorded id, plus the two found by name; none twice
        self.assertFalse((claims / '5.machines').exists())

    def test_record_needs_your_claim(self):
        self.env.claim('linuxpool', 5, owner='/work/other')
        self.assertNotEqual(self.env.run('linuxpool', 'record', '5', REC).returncode, 0)

    def test_revoke_failure_still_stops_the_runner_but_fails_the_command(self):
        self.env.claim('linuxpool', 2)
        done = self.env.run('linuxpool', 'stop', '2', up='127.0.0.1:22402 ', STUB_CLEANUP_FAIL='1')
        self.assertNotEqual(done.returncode, 0)
        self.assertIn('cannot list machine records', done.stderr)
        self.assertTrue(any('linuxpool-2 ' in c and 'touch /tmp/linuxpool-stop' in c for c in self.env.calls()))

    def test_nothing_found_means_no_apply(self):
        self.env.claim('linuxpool', 2)
        stub = self.env.bin / 'cleanup'
        stub.write_text('#!/bin/bash\necho "cleanup $*" >> "$STUB_LOG"\necho "would revoke 0 machine(s)"\n')
        self.env.run('linuxpool', 'stop', '2', up='127.0.0.1:22402 ')
        self.assertFalse(any(c.endswith('--apply') for c in self.env.calls()))

    def test_release_revokes_and_keeps_the_runner(self):
        claims = self.env.claim('macpool', 4)
        done = self.env.run('macpool', 'release', '4', up='127.0.0.1:22304 ')
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertTrue(any(c.endswith('--apply') and '--slot 4' in c for c in self.env.calls()))
        self.assertFalse(any('touch /tmp/macpool-stop' in c for c in self.env.calls()))
        self.assertFalse((claims / '4').exists())

    def test_someone_elses_claim_is_never_revoked(self):
        self.env.claim('linuxpool', 7, owner='/work/other')
        done = self.env.run('linuxpool', 'stop', '7', up='127.0.0.1:22407 ')
        self.assertNotEqual(done.returncode, 0)
        self.assertFalse(any(c.startswith('cleanup') for c in self.env.calls()))

    def test_sweep_revokes_only_slots_that_are_down_and_not_starting(self):
        self.env.claim('linuxpool', 1)             # up: its runner is alive
        self.env.claim('linuxpool', 2, age_min=3)  # claimed three minutes ago: still starting
        self.env.claim('linuxpool', 3)             # down: its lease is over
        done = self.env.run('linuxpool', 'sweep', up='127.0.0.1:22401 ')
        self.assertEqual(done.returncode, 0, done.stderr)
        listing = next(c for c in self.env.calls() if c.startswith('cleanup'))
        slots = [int(w) for a, w in zip(listing.split(), listing.split()[1:]) if a == '--slot']
        self.assertNotIn(1, slots)
        self.assertNotIn(2, slots)
        self.assertEqual(slots, [3, 4, 5, 6, 7, 8, 9, 10, 11, 12])
        self.assertTrue(any(c.endswith('--apply') for c in self.env.calls()))

    def test_sweep_with_every_runner_up_does_nothing(self):
        up = '\n'.join(f'127.0.0.1:{22300 + n} ' for n in range(1, 9))
        done = self.env.run('macpool', 'sweep', up=up)
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertFalse(any(c.startswith('cleanup') for c in self.env.calls()))

    def test_start_revokes_leftovers_of_the_slots_it_takes(self):
        done = self.env.run('linuxpool', 'start', '1', '1', up='127.0.0.1:22401 \n127.0.0.1:22402 ')
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertTrue(any(c.startswith('cleanup --pool linuxpool --slot 3 ') for c in self.env.calls()), self.env.calls())
        self.assertTrue(any(c.startswith('gh workflow run linuxpool.yml') for c in self.env.calls()))

    def test_start_skips_a_slot_queued_in_the_second_repository(self):
        conf = self.env.home / '.config' / 'paperboat-dev'
        conf.mkdir(parents=True)
        (conf / 'linuxpool-gh.repo').write_text('org/runners')
        (conf / 'linuxpool-gh.token').write_text('t')
        done = self.env.run('macpool', 'start', '1', STUB_QUEUE_REPO='org/runners', STUB_QUEUE_JOB='macpool-1')
        self.assertIn('macpool-2', done.stdout, done.stderr)

    def test_start_skips_a_slot_claimed_until_its_lease_ends(self):
        self.env.run('macpool', 'start', '1', '2')
        claims = self.env.home / '.ssh' / 'macpool-claims'
        hour = os.stat(claims / '1').st_mtime - 3600  # an hour later its runner is down but the lease runs
        os.utime(claims / '1', (hour, hour))
        done = self.env.run('macpool', 'start', '1')
        self.assertIn('macpool-2', done.stdout, done.stderr)
        self.assertFalse(any('--slot 1 ' in c for c in self.env.calls()[-3:]))

    def test_revoking_can_be_switched_off(self):
        self.env.claim('linuxpool', 2)
        done = self.env.run('linuxpool', 'stop', '2', up='127.0.0.1:22402 ', POOL_REVOKE='0')
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertFalse(any(c.startswith('cleanup') for c in self.env.calls()))


class CleanupScopeTest(unittest.TestCase):
    def setUp(self):
        self.env = Env(self)
        self.targets = pathlib.Path(self.env.dir.name) / 'targets.json'
        self.targets.write_text(json.dumps({'deployment_host': 'db', 'cleanup_accounts': ['user_x'],
                                            'cleanup_protected': '(pbvm|^canary-)'}))

    def run_cleanup(self, *args):
        env = dict(os.environ, HOME=str(self.env.home), PATH=f'{self.env.bin}:{os.environ["PATH"]}', STUB_LOG=str(self.env.log),
                   STUB_SQL=str(self.env.sql), PB_DEV_TARGETS_FILE=str(self.targets), STUB_UP='')
        return subprocess.run(['python3', str(HERE / 'cleanup_test_account.py'), *args], env=env,
                              capture_output=True, text=True, timeout=60, cwd=HERE)

    def test_pool_slot_narrows_the_alias_and_keeps_every_other_fence(self):
        done = self.run_cleanup('--pool', 'macpool', '--slot', '3', '--slot', '12', '--id', ID1, '--stale', '0 seconds', '--apply')
        self.assertEqual(done.returncode, 0, done.stderr)
        sql = self.env.sql.read_text()
        self.assertIn("m.alias ~ '^macpool-(.+-)?(3|12)$'", sql)
        self.assertIn("m.alias !~* '(pbvm|^canary-)'", sql)
        self.assertIn("ARRAY['user_x']::text[]", sql)
        self.assertIn(f"ARRAY['{ID1}']::text[]", sql)
        self.assertIn("m.alias ~ '^(", sql)  # still a throwaway name
        self.assertIn('macpool-', sql)

    def test_pool_runner_names_are_throwaway(self):
        self.run_cleanup('--all', '--pool', 'linuxpool', '--slot', '1')
        self.assertRegex(self.env.sql.read_text(), r"m\.alias ~ '\^\([^']*linuxpool-[^']*macpool-")

    def test_without_pool_nothing_extra_is_required(self):
        self.run_cleanup('--prefix', 'test-')
        self.assertIn("m.alias ~ ''", self.env.sql.read_text())

    def test_pool_and_slot_go_together(self):
        for args in (['--pool', 'linuxpool'], ['--slot', '2'], ['--pool', 'linuxpool', '--slot', '0'], ['--pool', 'nope', '--slot', '1']):
            self.assertNotEqual(self.run_cleanup('--all', *args).returncode, 0, args)


if __name__ == '__main__':
    unittest.main()
