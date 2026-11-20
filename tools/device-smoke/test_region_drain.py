import threading
import unittest

import region_drain
from region_drain import Fail, run, totp, wait_until


class Clock:
    def __init__(self):
        self.t = 0.0

    def sleep(self, seconds):
        self.t += seconds

    def now(self):
        return self.t


class Operator:
    """Two nodes in one region; the node's record follows the operator's actions."""

    def __init__(self, clock, ends_on_move=True):
        self.clock, self.ends_on_move = clock, ends_on_move
        self.nodes = {n: {'id': n, 'state': 'ready', 'ready': True, 'revision': 1, 'carriers': 3} for n in ('a', 'b')}
        self.held_a = None
        self.actions = []

    def node(self, node_id, region=''):
        return self.nodes[node_id]

    def act(self, node_id, action):
        self.actions.append((node_id, action))
        node = self.nodes[node_id]
        if action in ('drain', 'drain-move'):
            node.update(state='draining', ready=False)
        if action == 'drain-move':
            node['carriers'] = 0
            if self.ends_on_move and self.held_a:
                self.held_a.alive = False
        if action == 'resume':
            node.update(state='ready', ready=True, carriers=3)


class Held:
    def __init__(self, alive=True):
        self.alive = alive

    def body(self):
        if not self.alive:
            raise ConnectionError('closed')
        return 'marker'


class Probe:
    def __init__(self, failures=0):
        self.failures, self.total, self.stop = [('t', 'x')] * failures, 30, threading.Event()

    def start(self):
        pass


class Env:
    def __init__(self, operator, clock, cut_by_drain=False, probe_failures=0, never_serves=False):
        self.operator, self.sleep, self.now = operator, clock.sleep, clock.now
        self.nodes, self.ips, self.marker = ['a', 'b'], {'a': '192.0.2.1', 'b': '192.0.2.2'}, 'marker'
        self.check_dns, self.serve_seconds, self.up_seconds, self.move_seconds = True, 10, 20, 150
        self.dns_seconds, self.settle_seconds, self.max_probe_failures = 90, 20, 0
        self.probe, self.cut_by_drain, self.never_serves, self.dns = Probe(probe_failures), cut_by_drain, never_serves, {'192.0.2.1', '192.0.2.2'}

    def pinned(self, node):
        if self.never_serves:
            raise ConnectionError('refused')
        return 'marker'

    def held(self, node):
        held = Held()
        self.operator.held_a = held
        return held

    def resolve(self):
        return {ip for node, ip in self.ips.items() if self.operator.nodes[node]['ready']}


class RegionDrain(unittest.TestCase):
    def setUp(self):
        self.clock = Clock()
        self.operator = Operator(self.clock)

    def test_totp_matches_the_rfc_vector(self):
        self.assertEqual(totp('GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ', now=59), '287082')

    def test_wait_until_reports_seconds_and_gives_up(self):
        clock = Clock()
        self.assertEqual(wait_until(lambda: clock.t >= 6, 30, step=2, sleep=clock.sleep, now=clock.now), 6.0)
        self.assertIsNone(wait_until(lambda: False, 10, step=2, sleep=clock.sleep, now=clock.now))

    def test_passes_when_drain_lets_existing_finish_and_move_ends_them(self):
        env = Env(self.operator, self.clock)
        receipt = run(env)
        self.assertEqual(self.operator.actions, [('a', 'drain'), ('a', 'resume'), ('a', 'drain-move'), ('a', 'resume')])
        self.assertIn('move-connection-ended', receipt['steps'])
        self.assertEqual(receipt['probe']['failures'], 0)

    def test_fails_when_move_leaves_the_connection_served(self):
        operator = Operator(self.clock, ends_on_move=False)
        with self.assertRaisesRegex(Fail, 'move: .*still served'):
            run(Env(operator, self.clock))

    def test_fails_when_plain_drain_cuts_the_connection(self):
        operator = self.operator
        original = operator.act

        def cutting(node_id, action):
            original(node_id, action)
            if action == 'drain':
                operator.held_a.alive = False
        operator.act = cutting
        with self.assertRaisesRegex(Fail, 'drain: .*existing connections must finish'):
            run(Env(operator, self.clock))

    def test_fails_when_visitors_saw_errors(self):
        with self.assertRaisesRegex(Fail, 'visitors: 2 of 30'):
            run(Env(self.operator, self.clock, probe_failures=2))

    def test_fails_when_a_node_never_serves(self):
        with self.assertRaisesRegex(Fail, 'serve-all'):
            run(Env(self.operator, self.clock, never_serves=True))

    def test_a_failed_run_resumes_the_drained_node(self):
        operator = Operator(self.clock, ends_on_move=False)
        with self.assertRaises(Fail):
            run(Env(operator, self.clock))
        self.assertEqual(operator.nodes['a']['state'], 'ready')


if __name__ == '__main__':
    unittest.main()
