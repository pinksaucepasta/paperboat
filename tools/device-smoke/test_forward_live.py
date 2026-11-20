import ssl
import unittest

import forward_live
from forward_live import links_verdicts, pair_verdict, parse_nodes, run


def fw(forwarded=0, accepted=0, links=2):
    return {'links': links, 'names': 1, 'forwarded': forwarded, 'accepted': accepted, 'holder_miss': 0,
            'unreachable': 0, 'loop_refused': 0, 'bytes_in': 0, 'bytes_out': 0}


class Env:
    """Two edges whose counters live in `self.counters`; the fake probe bumps them like a real forwarded request would."""

    def __init__(self, bump=True, probe=None, check_links=False, relays=()):
        self.edges, self.publications = {'a': 'ip-a', 'b': 'ip-b'}, {'one.example.test': 'a', 'two.example.test': 'b'}
        self.relays, self.check_links, self.status, self.settle_seconds = list(relays), check_links, 200, 10
        self.counters, self.bump, self.t = {'a': fw(), 'b': fw()}, bump, 0.0
        self.probe = probe or self.served

    def served(self, ip, name):
        x, y = ('a', 'b') if ip == 'ip-a' else ('b', 'a')
        if self.bump:
            self.counters[x]['forwarded'] += 1
            self.counters[y]['accepted'] += 1
        return 200

    def stats(self):
        return {n: dict(c) for n, c in self.counters.items()}

    def sleep(self, seconds):
        self.t += seconds

    def now(self):
        return self.t


class Pair(unittest.TestCase):
    def test_parse_nodes_keeps_absent_forward_as_none(self):
        page = {'data': {'items': [{'id': 'a', 'forward': fw()}, {'id': 'b'}]}}
        self.assertEqual(parse_nodes(page), {'a': fw(), 'b': None})

    def test_forward_accepted_and_no_loop_passes(self):
        ok, _ = pair_verdict({'x': fw(1), 'y': fw(0, 1)}, {'x': fw(2), 'y': fw(0, 2)}, 'x', 'y')
        self.assertTrue(ok)

    def test_holder_that_forwarded_again_fails(self):
        ok, detail = pair_verdict({'x': fw(1), 'y': fw(0, 1)}, {'x': fw(2), 'y': fw(1, 2)}, 'x', 'y')
        self.assertFalse(ok)
        self.assertIn('twice', detail)

    def test_counter_that_did_not_rise_fails(self):
        ok, detail = pair_verdict({'x': fw(1), 'y': fw(0, 1)}, {'x': fw(1), 'y': fw(0, 1)}, 'x', 'y')
        self.assertFalse(ok)
        self.assertIn('x.forwarded', detail)
        self.assertIn('y.accepted', detail)

    def test_node_without_counters_fails(self):
        ok, detail = pair_verdict({'x': None, 'y': fw()}, {'x': None, 'y': fw()}, 'x', 'y')
        self.assertFalse(ok)
        self.assertIn('no forward counters', detail)


class Links(unittest.TestCase):
    def test_edges_need_every_peer_and_relays_one_link(self):
        rows = links_verdicts({'a': fw(links=1), 'b': fw(links=0), 'r': fw(links=1), 's': None}, ['a', 'b'], ['r', 's'])
        self.assertEqual([r[1] for r in rows], [True, False, True, False])


class Run(unittest.TestCase):
    def test_all_pairs_pass_and_relay_data_path_is_skipped(self):
        rows = run(Env())
        self.assertEqual([r[:2] for r in rows], [('pair a>b', 'PASS'), ('pair b>a', 'PASS'), ('relay-data-path', 'SKIP')])

    def test_counters_that_never_rise_fail_after_the_settle_time(self):
        rows = run(Env(bump=False))
        self.assertEqual([r[1] for r in rows[:2]], ['FAIL', 'FAIL'])

    def test_certificate_for_another_name_fails(self):
        def refuse(ip, name):
            raise ssl.SSLCertVerificationError('bad')
        rows = run(Env(probe=refuse))
        self.assertEqual(rows[0][1], 'FAIL')
        self.assertIn('SSLCertVerificationError', rows[0][2])

    def test_wrong_status_fails(self):
        self.assertEqual(run(Env(probe=lambda ip, name: 502))[0][1], 'FAIL')

    def test_pair_without_a_publication_is_skipped_and_nothing_checked_fails(self):
        env = Env()
        env.publications = {}
        rows = run(env)
        self.assertEqual([r[1] for r in rows], ['SKIP', 'SKIP', 'FAIL', 'SKIP'])

    def test_check_links_rows_are_added(self):
        rows = run(Env(check_links=True, relays=['r']))
        self.assertIn('links r', [r[0] for r in rows])
        self.assertEqual(next(r[1] for r in rows if r[0] == 'links r'), 'FAIL')  # r reports no counters


class Cli(unittest.TestCase):
    def test_edge_flag_needs_two_nodes(self):
        with self.assertRaises(SystemExit):
            forward_live.main(['--api', 'https://api.example.test', '--cred', 'cred', '--edge', 'a=192.0.2.1'])


if __name__ == '__main__':
    unittest.main()
