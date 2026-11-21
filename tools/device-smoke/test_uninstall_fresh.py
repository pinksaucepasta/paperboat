"""uninstall_fresh.py with a stubbed ssh/docker: leftovers of every category are listed, teardown runs on failure, no token leaks."""
import io
import json
import unittest
from contextlib import redirect_stderr, redirect_stdout
from unittest.mock import patch

import uninstall_fresh as uf

ID1 = 'machine_11111111-1111-1111-1111-111111111111'
ID2 = 'machine_22222222-2222-2222-2222-222222222222'
TOKEN = 'ABCDEFGHJKLMNPQRSTUVWXYZ12'
DIGEST = 'a' * 64 + '  -'


class Host:
    """Answers the commands uninstall_fresh sends, by what they run. Knobs make one stage misbehave."""

    def __init__(self, **knobs):
        self.knobs, self.commands, self.revoked, self.pairs = knobs, [], [], 0

    def ssh(self, host, command, stdin=None, timeout=300):
        self.commands.append(command)
        k = self.knobs
        if 'rm -f' in command and ' run -d' not in command and 'exec' not in command:
            return 0, ''
        if ' run -d ' in command:
            return (1, 'no such image') if k.get('no_container') else (0, '')
        if command == 'bash -s':
            self.pairs += 1
            if k.get('pair_fails'):
                return 1, "enrollment refused PB_TOKEN='%s'" % TOKEN
            return 0, 'MACHINE_ID=%s\n' % (ID1, ID2)[self.pairs - 1]
        if 'getent passwd' in command:
            extra = '\npbguard' if k.get('account') and self.uninstalled else ''
            return 0, 'root\npbt\n--\nroot\npbt' + extra
        if 'machine-registration.json' in command:
            return 0, '/home/pbt/Paperboat Inbox\n'
        if 'sha256sum' in command:
            return 0, DIGEST if not (k.get('inbox_lost') and self.uninstalled) else 'bad'
        if 'cat > /root/uninstall_inventory.py' in command:
            return 0, ''
        if 'uninstall_inventory.py --user' in command:
            if not self.uninstalled:
                return 1, json.dumps({'status': 'FAIL', 'total': 3, 'leftovers': {'files': 3}})
            if k.get('inventory'):
                return 1, json.dumps({'status': 'FAIL', 'total': 2, 'leftovers': {'services': 1, 'processes': 1}, 'file_paths': ['/var/lib/paperboat']})
            return 0, json.dumps({'status': 'PASS', 'total': 0, 'leftovers': {}})
        if 'uninstall --confirmation' in command:
            self.uninstalled = True
            # the real `pb uninstall --json` shape (cli/cmd/pb/uninstall_command.go uninstallJSONResult, wrapped by writeCLIJSON)
            data = {'hostname': 'h', 'inbox_preserved': True, 'uninstalled': not k.get('uninstall_refused')}
            return 0, json.dumps({'schema_version': '1.0', 'ok': True, 'data': data} if not k.get('flat') else data)
        if 'INBOX=${INBOX:?}' in command:
            return 0, ''.join('LEFT\t%s\t%s\n' % kv for kv in k.get('scan', []))
        if 'machine revoke' in command:
            if '--confirm' not in command:
                return 0, json.dumps({'ok': True, 'data': {'confirmation_token': 'AB12CD'}})
            self.revoked.append(command.split('machine revoke ')[1].split()[0])
            return 0, json.dumps({'ok': True, 'data': {'revoked': True}})
        if 'machine list --all' in command:
            listed = [{'id': ID1, 'state': 'online'}] if k.get('still_listed') and ID1 not in self.revoked else []
            listed += [{'id': ID2, 'state': 'online'}] if k.get('revoke_ineffective') else []
            return 0, json.dumps({'machines': listed})
        if 'owner=%U' in command:
            self.triaged = command
            return 0, '== /root/.local/state/paperboat\nowner=root:root'
        if 'doctor --json' in command:
            return 0, json.dumps({'overall': 'unhealthy' if k.get('unhealthy') else 'healthy'})
        return 0, ''

    uninstalled = False


def run(**knobs):
    host = Host(**knobs)
    out = io.StringIO()
    clock = iter(range(0, 10 ** 6, 40))  # each monotonic() read jumps 40 s, so the doctor wait ends
    with patch.object(uf, 'run_ssh', host.ssh), patch.object(uf, 'stage', lambda *a: True), \
            patch.object(uf.time, 'sleep', lambda s: None), patch.object(uf.time, 'monotonic', lambda: next(clock)), \
            redirect_stdout(out):
        code = uf.main(['uninstall_fresh.py', 'host', '/bin/pb'])
    return code, json.loads(out.getvalue()), host, out.getvalue()


class UninstallFresh(unittest.TestCase):
    def test_clean_uninstall_passes_with_both_machines_revoked_and_container_removed(self):
        code, result, host, _ = run()
        self.assertEqual((code, result['status'], result['leftovers']), (0, 'PASS', []))
        self.assertEqual(set(result), {'check', 'status', 'leftovers', 'machine_id', 'machine_ids', 'before', 'reinstall', 'container'})
        self.assertEqual((result['check'], result['machine_id'], result['machine_ids'], result['reinstall']), ('uninstall-fresh', ID1, [ID1, ID2], 'doctor healthy'))
        self.assertEqual(host.revoked, [ID1, ID2])
        self.assertTrue(any(c.startswith('sudo -n docker rm -f test-pbu1-') for c in host.commands))
        self.assertEqual(result['before'], {'files': 3})  # the inventory saw the install before the uninstall

    def test_every_category_of_leftover_is_listed_without_stopping(self):
        scan = [('binary', '/home/pbt/.local/bin/pb'), ('unit-file', '/etc/systemd/system/paperboatd.service'), ('unit', 'user:paperboatd.service'),
                ('socket', '/run/paperboat/x.sock'), ('process', '42 pb'), ('listener', 'tcp LISTEN pb'), ('file', '/var/lib/paperboat')]
        code, result, host, _ = run(scan=scan, inventory=True, account=True, inbox_lost=True, still_listed=True, unhealthy=True)
        self.assertEqual((code, result['status']), (1, 'FAIL'))
        found = '\n'.join(result['leftovers'])
        for expected in ('binary:/home/pbt/.local/bin/pb', 'unit-file:/etc/systemd/system/paperboatd.service', 'unit:user:paperboatd.service',
                         'socket:/run/paperboat/x.sock', 'process:42 pb', 'listener:tcp LISTEN pb', 'file:/var/lib/paperboat',
                         'inventory:services=1', 'inventory:processes=1', 'account:pbguard', 'inbox:', 'server-record: ',
                         'reinstall: doctor was not healthy'):
            self.assertIn(expected, found)
        self.assertEqual(host.revoked, [ID1, ID2])  # the reinstall still ran and both machines were revoked

    def test_setup_failure_still_removes_the_container_and_stays_fail(self):
        code, result, host, printed = run(pair_fails=True)
        self.assertEqual((code, result['status']), (1, 'FAIL'))
        self.assertIn('install+pair', result['leftovers'][0])
        self.assertTrue(any(' rm -f test-pbu1-' in c for c in host.commands))
        self.assertEqual(host.revoked, [])
        self.assertNotIn(TOKEN, printed)

    def test_container_that_does_not_start_is_a_fail_and_is_cleaned_up(self):
        code, result, host, _ = run(no_container=True)
        self.assertEqual((code, result['status']), (1, 'FAIL'))
        self.assertTrue(any(' rm -f test-pbu1-' in c for c in host.commands))

    def test_a_revoke_that_leaves_the_machine_listed_is_a_leftover(self):
        code, result, host, _ = run(revoke_ineffective=True)
        self.assertEqual((code, result['status']), (1, 'FAIL'))
        self.assertIn('machine %s is still listed' % ID2, ' '.join(result['leftovers']))
        self.assertEqual(host.revoked, [ID1, ID2])  # both were revoked by exact id, with the printed confirmation

    def test_real_wrapped_uninstall_result_is_not_a_leftover_and_flat_is_accepted(self):
        for knobs in ({}, {'flat': True}):
            self.assertEqual(run(**knobs)[1]['leftovers'], [])
        code, result, _, _ = run(uninstall_refused=True)
        self.assertIn('pb uninstall: exit 0', ' '.join(result['leftovers']))

    def test_leftover_files_are_triaged_on_stderr_before_teardown(self):
        err = io.StringIO()
        with redirect_stderr(err):
            code, result, host, _ = run(scan=[('file', '/root/.local/state/paperboat'), ('file', '/run/user/1000/paperboat')])
        self.assertIn('owner=root:root', err.getvalue())
        self.assertIn("/root/.local/state/paperboat /run/user/1000/paperboat", host.triaged)

    def test_keep_leaves_container_and_machines(self):
        host = Host()
        err = io.StringIO()
        with patch.object(uf, 'run_ssh', host.ssh), patch.object(uf, 'stage', lambda *a: True), patch.object(uf.time, 'sleep', lambda s: None), \
                redirect_stdout(io.StringIO()), redirect_stderr(err):
            uf.main(['x', 'h', 'b', '--keep'])
        self.assertIn('kept container test-pbu1-', err.getvalue())
        self.assertEqual(host.revoked, [])
        self.assertEqual(sum('docker rm -f test-pbu1-' in c for c in host.commands), 1)  # only the pre-run clean-up, no teardown

    def test_redact_removes_tokens(self):
        text = uf.redact("PB_TOKEN='%s' and %s" % (TOKEN, TOKEN))
        self.assertNotIn(TOKEN, text)


if __name__ == '__main__':
    unittest.main()
