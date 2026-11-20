import ssl
import unittest
from unittest import mock

import forward_live
from forward_live import TCP_PROBE, links_verdicts, pair_verdict, parse_nodes, run, status_of, tcp_echo


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
        self.tcp_name, self.tcp_port, self.tcp_echo = None, None, None
        self.terminal_host, self.terminal_origin, self.tickets, self.ws_status = None, None, [], None

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


def tcp_env(holder='b', echo_ok=True, forwards=True):
    """Edges a and b, a raw TCP tunnel named tunnel.example.test held by `holder`; echoes like an origin behind the holder."""
    env = Env()
    env.publications = {'tunnel.example.test': holder}
    env.tcp_name, env.tcp_port = 'tunnel.example.test', 2222

    def echo(ip, port):
        x = 'a' if ip == 'ip-a' else 'b'
        if forwards and x != holder:
            env.counters[x]['forwarded'] += 1
            env.counters[holder]['accepted'] += 1
        return echo_ok
    env.tcp_echo = echo
    return env


def term_env(statuses, tickets):
    """Edges a and b; statuses[(ip, sent a ticket)] is the upgrade's answer."""
    env = Env()
    env.terminal_host, env.terminal_origin, env.tickets = 'runtime.example.test', 'https://app.example.test', tickets
    env.ws_status = lambda ip, host, origin, subprotocol=None: statuses[(ip, subprotocol is not None)]
    return env


class FakeSock:
    """Canned replies for recv; everything sent is kept."""

    def __init__(self, replies):
        self.replies, self.sent = list(replies), b''

    def sendall(self, data):
        self.sent += data

    def recv(self, size):
        return self.replies.pop(0) if self.replies else b''

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


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
        self.assertEqual([r[:2] for r in rows], [('terminal-any-edge', 'SKIP'), ('pair a>b', 'PASS'), ('pair b>a', 'PASS'),
                                                 ('relay-data-path', 'SKIP')])

    def test_counters_that_never_rise_fail_after_the_settle_time(self):
        rows = run(Env(bump=False))
        self.assertEqual([r[1] for r in rows[1:3]], ['FAIL', 'FAIL'])

    def test_certificate_for_another_name_fails(self):
        def refuse(ip, name):
            raise ssl.SSLCertVerificationError('bad')
        rows = run(Env(probe=refuse))
        self.assertEqual(rows[1][1], 'FAIL')
        self.assertIn('SSLCertVerificationError', rows[1][2])

    def test_wrong_status_fails(self):
        self.assertEqual(run(Env(probe=lambda ip, name: 502))[1][1], 'FAIL')

    def test_pair_without_a_publication_is_skipped_and_nothing_checked_fails(self):
        env = Env()
        env.publications = {}
        rows = run(env)
        self.assertEqual([r[1] for r in rows], ['SKIP', 'SKIP', 'SKIP', 'FAIL', 'SKIP'])

    def test_check_links_rows_are_added(self):
        rows = run(Env(check_links=True, relays=['r']))
        self.assertIn('links r', [r[0] for r in rows])
        self.assertEqual(next(r[1] for r in rows if r[0] == 'links r'), 'FAIL')  # r reports no counters


class Tcp(unittest.TestCase):
    def test_echo_back_and_forwarded_by_the_non_holder_passes(self):
        rows = [r for r in run(tcp_env(holder='b')) if r[0].startswith('tcp')]
        self.assertEqual([r[1] for r in rows], ['PASS', 'PASS'])  # a forwards to b; b, the holder, forwards nothing

    def test_missing_echo_fails_without_waiting_for_counters(self):
        env = tcp_env(echo_ok=False)
        rows = [r for r in run(env) if r[0].startswith('tcp')]
        self.assertEqual([r[1] for r in rows], ['FAIL', 'FAIL'])
        self.assertEqual(env.t, 0)

    def test_non_holder_whose_forwarded_did_not_rise_fails_after_the_settle_time(self):
        rows = [r for r in run(tcp_env(holder='b', forwards=False)) if r[0].startswith('tcp')]
        self.assertEqual(rows[0][1], 'FAIL')
        self.assertIn('a.forwarded did not rise', rows[0][2])

    def test_holder_unknown_is_skipped_and_nothing_is_dialed(self):
        env = tcp_env()
        env.publications = {}
        env.tcp_echo = lambda ip, port: self.fail('the tunnel must not be dialed without a holder')
        rows = [r for r in run(env) if r[0].startswith('tcp')]
        self.assertEqual([r[1] for r in rows], ['SKIP', 'SKIP'])

    def test_tcp_echo_returns_the_probe_line_in_pieces(self):
        sock = FakeSock([TCP_PROBE[:5], TCP_PROBE[5:]])
        with mock.patch('forward_live.socket.create_connection', return_value=sock):
            self.assertTrue(tcp_echo('ip-a', 2222))
        self.assertEqual(sock.sent, TCP_PROBE)

    def test_tcp_echo_of_other_bytes_or_silence_is_false(self):
        with mock.patch('forward_live.socket.create_connection', return_value=FakeSock([b'nope'])):
            self.assertFalse(tcp_echo('ip-a', 2222))
        with mock.patch('forward_live.socket.create_connection', return_value=FakeSock([])):
            self.assertFalse(tcp_echo('ip-a', 2222))


class Terminal(unittest.TestCase):
    def test_101_with_ticket_and_4xx_without_passes_on_every_edge(self):
        env = term_env({('ip-a', True): 101, ('ip-a', False): 403, ('ip-b', True): 101, ('ip-b', False): 401},
                       ['t-a', 't-b'])
        rows = [r for r in run(env) if r[0].startswith('terminal')]
        self.assertEqual([r[:2] for r in rows], [('terminal-any-edge a', 'PASS'), ('terminal-any-edge b', 'PASS')])

    def test_upgrade_without_ticket_that_reaches_101_fails(self):
        env = term_env({('ip-a', True): 101, ('ip-a', False): 101, ('ip-b', True): 101, ('ip-b', False): 403}, ['t-a', 't-b'])
        row = run(env)[0]
        self.assertEqual(row[1], 'FAIL')
        self.assertIn('want a 4xx', row[2])

    def test_upgrade_with_ticket_refused_fails(self):
        env = term_env({('ip-a', True): 403, ('ip-a', False): 403, ('ip-b', True): 101, ('ip-b', False): 403}, ['t-a', 't-b'])
        row = run(env)[0]
        self.assertEqual(row[1], 'FAIL')
        self.assertIn('want 101', row[2])

    def test_ticket_file_shorter_than_the_edges_fails_naming_the_edge(self):
        env = term_env({('ip-a', True): 101, ('ip-a', False): 403, ('ip-b', True): 101, ('ip-b', False): 403}, ['t-a'])
        rows = run(env)[:2]
        self.assertEqual([r[:2] for r in rows], [('terminal-any-edge a', 'PASS'), ('terminal-any-edge b', 'FAIL')])
        self.assertIn('line 2', rows[1][2])

    def test_certificate_for_another_name_fails_the_edge(self):
        def refuse(ip, host, origin, subprotocol=None):
            raise ssl.SSLCertVerificationError('bad')
        env = term_env({}, ['t-a', 't-b'])
        env.ws_status = refuse
        row = run(env)[0]
        self.assertEqual(row[1], 'FAIL')
        self.assertIn('SSLCertVerificationError', row[2])

    def test_not_requested_is_skipped(self):
        self.assertEqual(run(Env())[0], ('terminal-any-edge', 'SKIP', 'not requested'))

    def test_status_line_code(self):
        self.assertEqual(status_of(b'HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n\r\n'), 101)
        self.assertEqual(status_of(b'HTTP/1.1 403 Forbidden\r\n\r\n'), 403)
        self.assertIsNone(status_of(b''))
        self.assertIsNone(status_of(b'SSH-2.0-x\r\n'))


class Cli(unittest.TestCase):
    def test_edge_flag_needs_two_nodes(self):
        with self.assertRaises(SystemExit):
            forward_live.main(['--api', 'https://api.example.test', '--cred', 'cred', '--edge', 'a=192.0.2.1'])

    def test_tcp_name_and_port_go_together(self):
        with self.assertRaises(SystemExit):
            forward_live.main(['--api', 'https://api.example.test', '--cred', 'cred', '--edge', 'a=192.0.2.1',
                               '--edge', 'b=192.0.2.2', '--tcp-port', '2222'])

    def test_terminal_options_go_together(self):
        with self.assertRaises(SystemExit):
            forward_live.main(['--api', 'https://api.example.test', '--cred', 'cred', '--edge', 'a=192.0.2.1',
                               '--edge', 'b=192.0.2.2', '--terminal-host', 'runtime.example.test'])


if __name__ == '__main__':
    unittest.main()
