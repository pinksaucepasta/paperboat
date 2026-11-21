"""Canary-gated deploy: green keeps, red or a failed deploy rolls back, red before refuses, lock always freed."""
import importlib.util
import json
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace

sys.path.insert(0, str(Path(__file__).parent))  # gated-deploy.py imports health_report from its own directory
spec = importlib.util.spec_from_file_location('gated', Path(__file__).with_name('gated-deploy.py'))
gated = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gated)

T0 = 1_800_000_000  # deploy starts here (wall clock seconds)


def status(ok, started):
    run = {'started': gated.iso(started), 'ok': ok,
           'steps': [{'step': 'exec_a_to_b', 'ok': ok}]}
    return {'updated': gated.iso(started), 'red': not ok, 'last': run}


class Fake:
    """Hosts with server and edge images; the deploy swaps them to new ones, a rollback swaps them back."""

    def __init__(self, canary_after_deploy, deploy_rc=0):
        self.clock = T0
        self.images = {'server': 'paperboat-server:1', 'local': ('paperboat-relay:1', 'paperboat-edge:1')}
        self.calls, self.lock, self.deployed = [], None, False
        self.after = list(canary_after_deploy)  # canary results (ok bools) for runs after the switch
        self.deploy_rc = deploy_rc
        self.rollout_error = ''  # when set, the server rollback command fails with this output
        self.migrated = False  # the deploy applied migrations: rollout-server.py refuses a rollback without --schema-skew-ok
        self.skew = {}  # (commit, context) -> GitHub status state
        self.alerts = []
        self.reports = []  # (source, payload) pushed to the admin panel

    def sh(self, target, script):
        self.calls.append((target, script))
        if 'noclobber' in script:
            if self.lock:
                return 1, ''
            self.lock = script
            return 0, ''
        if script.startswith('cat ') and 'deploy.lock' in script:
            return 0, 'W4 other deploy\n'
        if script.startswith('rm -f') and 'deploy.lock' in script:
            self.lock = None
            return 0, ''
        if 'server-rollout.json' in script:
            return 0, self.images['server'] + '\n'
        if 'docker ps' in script:
            relay, tunnel = self.images[target]
            return 0, f'paperboat-pbh-x-relay {relay}\npaperboat-pbh-x-edge {tunnel}\n'
        if 'image inspect' in script:
            return 0, ('1' if 'server:1' in script else '2') * 40 + '\n'
        if '--image' in script:
            if self.migrated and '--schema-skew-ok' not in script:
                return 3, 'Draining servers retained: 0\nRefused to roll back: the serving server\'s deploy migrated the schema'
            if self.rollout_error:
                return 1, 'Draining servers retained: 1\n' + self.rollout_error
            self.images['server'] = script.rsplit('--image ', 1)[1].strip()
            return 0, ''
        if 'pbh install' in script:
            parts = script.split()
            self.images[target] = (parts[parts.index('--relay-image') + 1], parts[parts.index('--edge-image') + 1])
            return 0, ''
        raise AssertionError(script)

    def deploy(self):
        self.deployed = True
        self.images = {'server': 'paperboat-server:2', 'local': ('paperboat-relay:1', 'paperboat-edge:2')}
        self.clock += 60
        return self.deploy_rc

    def canary(self):
        self.clock += 60
        if not self.deployed:
            return status(True, self.clock)
        return status(self.after.pop(0) if self.after else False, self.clock)


def gate(fake, status_file, force='', wait=300, stage=None):
    args = SimpleNamespace(what='W4 server 2', server_host='root@control', directory='/opt/paperboat',
                           edge=['local'], wait=wait, force=force, canary_status=status_file)
    out = []
    g = gated.Gate(args, fake.sh, fake.deploy, fake.canary, wall=lambda: fake.clock, mono=lambda: fake.clock,
                   say=out.append, stage=stage, skew_status=lambda c, ctx: fake.skew.get((c, ctx), ''),
                   alert=fake.alerts.append, report=lambda *r: fake.reports.append(r))
    return g.run(), out


class GatedDeployTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.status = Path(self.dir.name) / 'status.json'
        self.write(status(True, T0 - 30))

    def tearDown(self):
        self.dir.cleanup()

    def write(self, value):
        self.status.write_text(json.dumps(value))

    def test_green_after_switch_keeps_the_deploy(self):
        fake = Fake([True])
        code, out = gate(fake, self.status)
        self.assertEqual(code, 0)
        self.assertEqual(fake.images['server'], 'paperboat-server:2')
        self.assertFalse(any('--image' in s or 'pbh install' in s for _, s in fake.calls))
        self.assertIsNone(fake.lock)
        self.assertTrue(out[-1].startswith('kept'))
        self.assertEqual([(s, p['rolled_back']) for s, p in fake.reports], [('deploy', False)])  # clears the panel alert

    def test_access_pass_does_not_override_a_failed_account_probe(self):
        fake = Fake([True] * 5)
        def mixed_status():
            result = fake.canary()
            result['red'] = True
            return result
        args = SimpleNamespace(wait=300)
        g = gated.Gate(args, fake.sh, fake.deploy, mixed_status,
                       wall=lambda: fake.clock, mono=lambda: fake.clock)
        green, _ = g.wait_green(T0)
        self.assertFalse(green)

    def test_one_red_run_then_green_within_the_wait_keeps(self):
        fake = Fake([False, True])
        self.assertEqual(gate(fake, self.status)[0], 0)
        self.assertEqual(fake.images['server'], 'paperboat-server:2')

    def test_red_after_switch_rolls_back_server_and_edge_together(self):
        fake = Fake([False] * 5 + [True])
        code, out = gate(fake, self.status)
        self.assertEqual(code, 1)
        self.assertEqual(fake.images, {'server': 'paperboat-server:1',
                                       'local': ('paperboat-relay:1', 'paperboat-edge:1')})
        rollback = [s for _, s in fake.calls if '--image' in s or 'pbh install' in s]
        self.assertEqual(len(rollback), 2)
        (source, early), (_, report) = fake.reports  # the admin panel shows the rollback before and after the canary verdict
        self.assertNotIn('green_after', early)
        self.assertEqual((source, report['rolled_back'], report['green_after'], report['rollback_ok'], report['what']),
                         ('deploy', True, True, True, 'W4 server 2'))
        self.assertIn('no green canary run', report['reason'])
        self.assertIn('--reap', rollback[0])  # server first, reaped so retained generations never block it
        self.assertIn('--rollback --image', rollback[0])  # two draining servers never refuse a rollback
        self.assertIsNone(fake.lock)
        self.assertTrue(any('no green canary run within 300 s' in line for line in out))

    def test_server_rollback_that_fails_on_a_retained_draining_server_is_loud_and_exits_2(self):
        # 2026-11-12: 'Draining servers retained: 1 / Replacement container name already exists' left prod on the failed build.
        fake = Fake([False] * 5 + [True])
        fake.rollout_error = 'Replacement container name already exists; nothing was removed.'
        code, out = gate(fake, self.status)
        self.assertEqual(code, 2)
        self.assertEqual(fake.images['server'], 'paperboat-server:2')  # still on the failed build, said so below
        self.assertTrue(any('rollback: server rollout failed (exit 1)' in line and 'name already exists' in line for line in out))
        self.assertTrue(any('a rollback step failed' in line for line in out))
        rollout = [s for _, s in fake.calls if '--image' in s or '--reap' in s]
        self.assertIn('--reap', rollout[0])  # the draining servers are reaped first; a retained one is reported, never forced

    def test_red_after_migrations_without_schema_skew_pass_keeps_the_server_and_alerts(self):
        # 2026-11-21 07:54 IST: the rollback put .917 on the migrated schema (503s, edge 401s).
        fake = Fake([False] * 20)
        fake.migrated = True
        code, out = gate(fake, self.status)
        self.assertEqual(code, 6)
        self.assertEqual(fake.images['server'], 'paperboat-server:2')
        self.assertEqual(len(fake.alerts), 1)
        self.assertIn('SERVER KEPT', fake.alerts[0])
        self.assertIn('Roll forward', fake.alerts[0])
        self.assertEqual([(s, p['rolled_back'], p['kept_migrated']) for s, p in fake.reports], [('deploy', False, True)] * 2)
        self.assertTrue(any('roll forward' in line for line in out))
        self.assertIsNone(fake.lock)

    def test_red_after_migrations_with_schema_skew_pass_rolls_back(self):
        fake = Fake([False] * 5 + [True])
        fake.migrated = True
        fake.skew[('2' * 40, 'paperboat/schema-skew/' + '1' * 12)] = 'success'
        code, _ = gate(fake, self.status)
        self.assertEqual(code, 1)
        self.assertEqual(fake.images['server'], 'paperboat-server:1')
        self.assertEqual(fake.alerts, [])

    def test_rollback_that_stays_red_exits_2(self):
        fake = Fake([False] * 20)
        self.assertEqual(gate(fake, self.status)[0], 2)
        self.assertEqual(fake.images['server'], 'paperboat-server:1')

    def test_failed_deploy_command_rolls_back_without_waiting(self):
        fake = Fake([True], deploy_rc=1)
        code, out = gate(fake, self.status)
        self.assertEqual(code, 1)
        self.assertEqual(fake.images['server'], 'paperboat-server:1')
        self.assertTrue(any('the deploy command failed (exit 1)' in line for line in out))

    def test_failed_staging_refuses_before_prod_or_the_lock_is_touched(self):
        fake = Fake([True])
        code, out = gate(fake, self.status, stage=lambda: 7)
        self.assertEqual(code, 5)
        self.assertEqual((fake.calls, fake.deployed), ([], False))
        self.assertIn('staging step failed (exit 7)', out[0])

    def test_green_staging_lets_the_prod_deploy_run(self):
        fake = Fake([True])
        self.assertEqual(gate(fake, self.status, stage=lambda: 0)[0], 0)
        self.assertTrue(fake.deployed)

    def test_red_before_refuses_and_touches_nothing(self):
        self.write(status(False, T0 - 30))
        fake = Fake([True])
        code, out = gate(fake, self.status)
        self.assertEqual(code, 3)
        self.assertFalse(fake.deployed)
        self.assertEqual(fake.calls, [])
        self.assertIn('exec_a_to_b', out[0])

    def test_red_before_with_force_deploys_and_records_the_reason(self):
        self.write(status(False, T0 - 30))
        fake = Fake([True])
        code, out = gate(fake, self.status, force='deploying the exec fix')
        self.assertEqual(code, 0)
        self.assertTrue(fake.deployed)
        self.assertIn('forced: deploying the exec fix', next(s for _, s in fake.calls if 'noclobber' in s))

    def test_stale_status_runs_the_canary_first(self):
        self.write(status(True, T0 - 3600))
        fake = Fake([True])
        self.assertEqual(gate(fake, self.status)[0], 0)
        self.missing = Path(self.dir.name) / 'none.json'
        self.assertEqual(gate(Fake([True]), self.missing)[0], 0)

    def test_held_lock_refuses_without_deploying_or_freeing_it(self):
        fake = Fake([True])
        fake.lock = 'someone'
        code, out = gate(fake, self.status)
        self.assertEqual(code, 4)
        self.assertFalse(fake.deployed)
        self.assertEqual(fake.lock, 'someone')
        self.assertIn('W4 other deploy', out[0])

    def test_unreadable_images_free_the_lock(self):
        fake = Fake([True])
        fake.images['local'] = ('', '')
        fake.sh_orig = fake.sh
        fake.sh = lambda t, s: (0, '') if 'docker ps' in s else fake.sh_orig(t, s)
        with self.assertRaises(RuntimeError):
            gate(fake, self.status)
        self.assertIsNone(fake.lock)
        self.assertFalse(fake.deployed)

    def test_parse_edge(self):
        self.assertEqual(gated.parse_edge('paperboat-pbh-1-relay r:1\npaperboat-pbh-1-edge t:2\nother x\n'),
                         {'relay': 'r:1', 'edge': 't:2'})


if __name__ == '__main__':
    unittest.main()
