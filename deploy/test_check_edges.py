"""check-edges.sh fails on every user-visible failure during a deploy (prod 2026-11-05: 22 refused host
carriers, a preview 503 and a machine_offline passed the old gate). Run: python3 deploy/test_check_edges.py"""
import os
import pathlib
import re
import shutil
import stat
import subprocess
import tempfile
import unittest

SCRIPT = pathlib.Path(__file__).with_name('check-edges.sh')

STUBS = {
    # The probe machine: answers the preview request, ignores the rest.
    'probe': '''#!/bin/sh
s=$(cat)
case "$s" in *create_connection*) [ -e "$T/origin-down" ] && exit 1 ;; esac
case "$s" in *"pb preview "*"--json"*) echo '{"data":{"id":"preview_1","url":"https://p1.preview.example.test"}}' ;; esac
''',
    # Read-only psql: online machines, machines offline since the start, 5xx count.
    'psql': '''#!/bin/sh
s=$(cat)
case "$s" in *default_transaction_read_only*) echo SET ;; esac
case "$s" in
*"state = 'online'"*) printf 'machine_1\\nmachine_2\\n' ;;
*"not online"*) cat "$T/offline" 2>/dev/null ;;
*support_reports*"code <> 'edge_error_page'"*) cat "$T/server5xx" ;;
*"select support_reference"*"code = 'edge_error_page'"*) echo support_aaaa ;;  # only support_aaaa is a visitor app error page
*support_reports*) echo 99 ;;  # a count that includes visitors' edge error pages fails every case
esac
''',
    'sudo': '#!/bin/sh\n[ "$1" = -n ] && shift\nexec "$@"\n',
    'docker': '''#!/bin/sh
case "$1" in
ps) echo container_new ;;
logs) cat "$T/edge.log" ;;
esac
''',
    'timeout': '#!/bin/sh\necho "$*" >>"$T/timeout.log"\nshift\nexec "$@"\n',
    'pkill': '#!/bin/sh\necho "$*" >>"$T/pkill.log"\n',
    'curl': '#!/bin/sh\nprintf "%s 0.010" "$(cat "$T/code")"\n',
}


class CheckEdgesTest(unittest.TestCase):
    def run_gate(self, edge_log='', server5xx='0', offline='', deploy='true', origin_down=False, extra_env=None):
        with tempfile.TemporaryDirectory() as directory:
            t = pathlib.Path(directory)
            bin_dir = t / 'bin'
            bin_dir.mkdir()
            for name, body in STUBS.items():
                path = bin_dir / name
                path.write_text(body)
                path.chmod(path.stat().st_mode | stat.S_IEXEC)
            (t / 'edge.log').write_text(edge_log)
            (t / 'server5xx').write_text(server5xx + '\n')
            (t / 'offline').write_text(offline)
            (t / 'code').write_text('200')
            if origin_down:
                (t / 'origin-down').write_text('')
            env = dict(os.environ, T=str(t), PATH=f'{bin_dir}:{os.environ["PATH"]}', PROBE=str(bin_dir / 'probe'),
                       SQL=str(bin_dir / 'psql'), SETTLE='2', OBSERVE='1', **(extra_env or {}))
            result = subprocess.run(['sh', str(SCRIPT), '--deploy', deploy, 'local=192.0.2.1'], env=env,
                                    capture_output=True, text=True, timeout=120)
            self.stub_logs = {n: (t / n).read_text() if (t / n).exists() else '' for n in ('timeout.log', 'pkill.log')}
            return result.returncode, result.stdout + result.stderr

    def test_clean_deploy_passes(self):
        code, out = self.run_gate()
        self.assertEqual(code, 0, out)
        self.assertIn('edge gate: PASS', out)

    def test_refused_host_carrier_fails(self):
        code, out = self.run_gate(edge_log='{"level":"WARN","msg":"carrier_connection_failed","schema":"paperboat.edge_event.v1",'
                                           '"reason":"unknown_machine"}\n')
        self.assertEqual(code, 1, out)
        self.assertIn('carrier=1', out)
        self.assertIn('unknown_machine', out)

    def test_existing_preview_failing_during_the_deploy_fails(self):
        code, out = self.run_gate(deploy='echo 503 > "$T/code"; sleep 2; echo 200 > "$T/code"')
        self.assertEqual(code, 1, out)
        self.assertRegex(out, r'preview failures=[1-9]')
        # each failed request names its time, edge, status, duration and path
        self.assertRegex(out, r'failed request: \d\d:\d\d:\d\d\.\d+ local 503 0\.010s GET p1\.preview\.example\.test/')
        kept = re.search(r'edge logs of the window kept in (\S+)', out)
        self.assertTrue(kept, out)
        self.assertTrue(pathlib.Path(kept.group(1), 'edge-local.log').exists())
        shutil.rmtree(kept.group(1))

    def test_kept_logs_are_limited_to_the_newest_few(self):
        with tempfile.TemporaryDirectory() as keep:
            for _ in range(4):
                code, out = self.run_gate(extra_env={'KEEP_LOGS': '1', 'KEEP_DIR': keep, 'KEEP_LAST': '2'})
                self.assertEqual(code, 0, out)
            kept = sorted(p.name for p in pathlib.Path(keep).glob('pb-gate-failed.*'))
            self.assertEqual(len(kept), 2, kept)

    def test_edge_5xx_and_attach_refusal_fail(self):
        code, out = self.run_gate(edge_log='{"msg":"edge_http_5xx","status":503}\n{"msg":"runtime_route_attach_refused","route_id":"r"}\n')
        self.assertEqual(code, 1, out)
        self.assertIn('attach=1 edge5xx=1', out)

    def test_visitor_app_error_pages_pass(self):
        code, out = self.run_gate(edge_log='{"msg":"edge_http_5xx","status":502,"reference":"support_aaaa"}\n')
        self.assertEqual(code, 0, out)
        self.assertIn('edge5xx=0', out)

    def test_edge_5xx_that_is_not_an_app_error_page_fails(self):
        code, out = self.run_gate(edge_log='{"msg":"edge_http_5xx","status":502,"reference":"support_aaaa"}\n'
                                  '{"msg":"edge_http_5xx","status":502,"reference":"support_bbbb"}\n')
        self.assertNotEqual(code, 0)
        self.assertIn('edge5xx=1', out)

    def test_server_5xx_fails(self):
        code, out = self.run_gate(server5xx='2')
        self.assertEqual(code, 1, out)
        self.assertIn('server: 5xx=2', out)

    def test_machine_going_offline_fails_and_others_do_not_count(self):
        code, out = self.run_gate(offline='machine_2\nmachine_never_online\n')
        self.assertEqual(code, 1, out)
        self.assertIn('offline during the deploy=1', out)
        self.assertNotIn('machine_never_online', out)

    def test_log_follower_is_bounded_and_killed_on_exit(self):
        code, out = self.run_gate()
        self.assertEqual(code, 0, out)
        self.assertIn('docker logs --follow', self.stub_logs['timeout.log'])
        self.assertIn('[d]ocker logs --follow --since', self.stub_logs['pkill.log'])

    def test_origin_that_never_listens_fails_before_publishing(self):
        code, out = self.run_gate(origin_down=True)
        self.assertEqual(code, 1, out)
        self.assertIn('probe origin never listened', out)

    def test_failed_deploy_command_fails(self):
        code, out = self.run_gate(deploy='exit 3')
        self.assertEqual(code, 1, out)
        self.assertIn('deploy command exit=3', out)


if __name__ == '__main__':
    unittest.main()
