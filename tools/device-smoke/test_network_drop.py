"""network_drop.py: the Unix client cut never takes the controller's SSH or a hosted runner with it, and proves it held."""
import os
import subprocess
import unittest
from pathlib import Path
from unittest.mock import patch

import network_drop

HERE = Path(__file__).resolve().parent
# `lsof -nP -a -u UID -iTCP -sTCP:ESTABLISHED -Fcn` on a hosted Mac (2026-11-28), trimmed
LSOF = '''p959
chosted-compute-agent
f6
n192.0.2.2:58660->192.0.2.23:443
p974
cRunner.Listener
f129
n[::ffff:192.0.2.2]:49153->[::ffff:198.51.100.105]:443
p1011
cssh
f3
n192.0.2.2:49159->203.0.113.60:22
f4
n127.0.0.1:58683->127.0.0.1:2222
p23962
cpb
f9
n192.0.2.2:56560->192.0.2.99:443
n[2001:db8::2]:50000->[2001:db8::1]:7272
'''


def done(out='', code=0, err=''):
    return subprocess.CompletedProcess([], code, out, err)


class MacCut(unittest.TestCase):
    def rules(self, listing):
        with patch.object(network_drop, 'sudo', return_value=done(listing)):
            return network_drop.pf_keep_rules()

    def test_every_connection_that_is_not_pbs_keeps_passing_to_that_address_and_port_only(self):
        uid = os.getuid()
        self.assertEqual(['pass out quick on ! lo0 proto tcp from any to %s port %d user %d flags any no state' % (address, port, uid)
                          for address, port in (('192.0.2.23', 443), ('198.51.100.105', 443), ('203.0.113.60', 22))],
                         self.rules(LSOF).splitlines())

    def test_a_destination_pb_shares_with_another_program_refuses_the_cut(self):
        with self.assertRaisesRegex(RuntimeError, 'cannot tell them apart'):
            self.rules(LSOF + 'p9\ncssh\nf3\nn192.0.2.2:1->192.0.2.99:443\n')

    def test_the_keep_rules_come_before_the_block(self):
        with patch.object(network_drop, 'DEVICE', None), patch.object(network_drop.winhost, 'PEER_CUT', []), \
                patch.object(network_drop, 'DARWIN', True), patch.object(network_drop, 'pf_keep_rules', return_value='pass quick\n'), \
                patch.object(network_drop, 'pf_block', return_value='block') as block:
            self.assertEqual(('block', None), network_drop.cut(480))
        block.assert_called_once_with('pass quick\n' + network_drop.PF_RULE, 480)

    def release(self, counters):
        with patch.object(network_drop.winhost, 'PEER_CUT', []), patch.object(network_drop, 'DARWIN', True), \
                patch.object(network_drop, 'sudo', return_value=done(counters)), patch.object(network_drop, 'pf_release') as lifted:
            try:
                return network_drop.release('block')
            finally:
                lifted.assert_called_once_with('block')

    def test_the_cut_is_proven_by_what_its_block_rules_dropped(self):
        counters = ('pass out quick on ! lo0 inet proto tcp from any to 203.0.113.60 port = 22 user = 501 flags any no state\n'
                    '  [ Evaluations: 90        Packets: 80        Bytes: 8000        States: 0     ]\n'
                    '  [ Inserted: uid 0 pid 1 State Creations: 0     ]\n'
                    'block drop out quick on ! lo0 proto tcp all user = 501\n'
                    '  [ Evaluations: 50        Packets: 7         Bytes: 700         States: 0     ]\n'
                    '  [ Inserted: uid 0 pid 1 State Creations: 0     ]\n'
                    'block drop out quick on ! lo0 proto udp all user = 501\n'
                    '  [ Evaluations: 40        Packets: 33        Bytes: 900         States: 0     ]\n')
        self.assertEqual('pf cut of uid %d: dropped=40' % os.getuid(), self.release(counters))
        for held_nothing in ('', counters.replace('Packets: 7 ', 'Packets: 0 ').replace('Packets: 33', 'Packets: 0 ')):
            with self.subTest(held_nothing[:20]), self.assertRaisesRegex(RuntimeError, 'not cut'):
                self.release(held_nothing)


class UnitCut(unittest.TestCase):
    """A real Linux machine: host_cut.sh cuts Paperboat's units and the check's cgroup, never an owner-drop."""

    def held(self, cgroups, out='cut: sent=0 dropped=12'):
        calls = []

        def run(argv, **kwargs):
            calls.append((argv, kwargs.get('env', {})))
            return done(out)
        patches = [patch.object(network_drop.winhost, 'PEER_CUT', []), patch.object(network_drop, 'DEVICE', None),
                   patch.object(network_drop, 'DARWIN', False), patch.object(network_drop, 'UNIT_CUT', True),
                   patch.object(Path, 'is_dir', lambda path: path.name in cgroups),
                   patch.object(network_drop.subprocess, 'run', run),
                   patch.object(network_drop, 'sudo', side_effect=AssertionError('no iptables rule on a real machine'))]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        return calls

    def test_the_cut_takes_the_checks_cgroup_and_the_guard_with_the_units_and_proves_itself(self):
        calls = self.held({network_drop.CLIENT_CGROUP, 'paperboat-machineguard.service'})
        block, failsafe = network_drop.cut(480)
        self.assertEqual('cut: sent=0 dropped=12', network_drop.release(block))
        self.assertIsNone(failsafe)  # a systemd timer on the machine, not a child of this process
        self.assertEqual([['local', str(os.getuid()), 'add', 'all'], ['local', str(os.getuid()), 'remove']],
                         [argv[1:] for argv, _ in calls])
        self.assertTrue(calls[0][0][0].endswith('host_cut.sh'))
        self.assertEqual(('480', network_drop.CLIENT_CGROUP + ' ' + network_drop.GUARD_CGROUP),
                         (calls[0][1]['PB_CUT_FAILSAFE'], calls[0][1]['PB_CUT_ALSO']))

    def test_a_cut_that_let_packets_through_is_not_a_cut(self):
        self.held({network_drop.CLIENT_CGROUP}, 'cut: sent=3 dropped=12')
        with self.assertRaisesRegex(RuntimeError, 'not cut'):
            network_drop.release(None)

    def test_without_its_own_cgroup_nothing_is_cut(self):
        calls = self.held(set())
        with self.assertRaisesRegex(RuntimeError, 'join_cgroup'):
            network_drop.cut(480)
        self.assertEqual([], calls)


class WindowsTarget(unittest.TestCase):
    """The held shell is cmd.exe: nothing is typed before its prompt, and each expected token is waited for once."""

    def test_network_drop_types_cmd_lines_into_a_windows_target(self):
        events = []

        class Cmd:
            def __init__(self, *args, **kwargs):
                self.output = ''

            def wait_windows_prompt(self):
                events.append('prompt')

            def send(self, text):
                events.append(text)
                self.output += text.replace('^', '').replace('%PB_DURABLE%', self.nonce) if '%PB_DURABLE%' in text else text.replace('^', '')
                if text.startswith('set PB_DURABLE='):
                    self.nonce = text.split('=', 1)[1].strip()

            def expect(self, token, timeout=45):
                if token not in self.output:  # like the real pty: a token is consumed by the wait that finds it
                    raise RuntimeError('terminal did not produce the expected fixture output: ' + token)
                self.output = self.output.split(token, 1)[1]

            def close(self):
                pass

        def run_pb(binary, arguments, timeout):
            read = 'Get-Content' in str(arguments) or 'powershell' in arguments
            return done(b''.join(b'IN_%d\r\n' % n for n in range(1, 8)) if read else b'')

        with patch.object(network_drop, 'Terminal', Cmd), patch.object(network_drop, 'DEVICE', None), \
                patch.object(network_drop.windows_target, 'is_windows', return_value=True), \
                patch.object(network_drop, 'require_cut_tool'), patch.object(network_drop, 'join_cgroup'), \
                patch.object(network_drop, 'cut', return_value=(None, None)), patch.object(network_drop, 'release'), \
                patch.object(network_drop.winhost, 'run_pb', run_pb), patch.object(network_drop.time, 'sleep'), \
                patch.object(network_drop, 'failures_since', return_value=1), patch.object(network_drop, 'cleanup'), \
                patch.object(network_drop.winhost, 'terminal_argv', return_value=['pb']), \
                patch.object(network_drop.winhost, 'terminal_env', return_value={}), \
                patch('sys.argv', ['network_drop.py', 'win-1']), patch('builtins.print'):
            network_drop.main()
        self.assertEqual('prompt', events[0])
        typed = ''.join(events[1:])
        self.assertNotIn('printf', typed)
        self.assertNotIn('PB_ACK_1001', typed)  # the typed line cannot match what the shell prints
        self.assertIn('echo PB_^ACK_1007', typed)


class LocalHostCut(unittest.TestCase):
    def test_local_runs_here_without_ssh_and_carries_the_extra_cgroups(self):
        import tempfile
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'ssh').write_text('#!/bin/sh\necho SSH-USED\nexit 9\n')
            # sudo records what it was given instead of running it
            (root / 'sudo').write_text('#!/bin/bash\n[ "$1 $2" = "-n true" ] && exit 0\nprintf "%s\\n" "$@" >"$OUT/args"; cat >/dev/null\n')
            for name in ('ssh', 'sudo'):
                (root / name).chmod(0o755)
            env = dict(os.environ, PATH=directory + os.pathsep + os.environ['PATH'], OUT=directory,
                       PB_CUT_ALSO='pbsmoke-client-1 system.slice/paperboat-machineguard.service')
            env.pop('PB_CUT_SUDO_FILE', None)
            result = subprocess.run([str(HERE / 'host_cut.sh'), 'local', '1000', 'add', 'all'], capture_output=True, text=True,
                                    env=env, timeout=30)
            self.assertEqual((0, ''), (result.returncode, result.stdout), result.stderr)
            args = (root / 'args').read_text()
            self.assertIn('PB_CUT_ALSO=pbsmoke-client-1 system.slice/paperboat-machineguard.service', args.splitlines())
            for host, bad in (('local', '../x'), ('local', 'a;reboot'), ('far', 'pbsmoke-client-1')):
                refused = subprocess.run([str(HERE / 'host_cut.sh'), host, '1000', 'add', 'all'], capture_output=True, text=True,
                                         env=dict(env, PB_CUT_ALSO=bad), timeout=30)
                self.assertEqual((2, ''), (refused.returncode, refused.stdout), (host, bad))


if __name__ == '__main__':
    unittest.main()
