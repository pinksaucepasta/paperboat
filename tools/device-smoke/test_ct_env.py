"""ct_env.sh / ct_env.py: the throwaway containers' image, user, uid and docker command come from SMOKE_CT_*, and nothing
else in the harness hardcodes them."""
import os
from pathlib import Path
import re
import subprocess
import sys
import unittest

HERE = Path(__file__).resolve().parent
DEFAULTS = {'CT_IMAGE': 'test-systemd', 'CT_USER': 'pbt', 'CT_UID': '1000', 'CT_DOCKER': 'sudo -n docker',
            'CT_HOME': '/home/pbt', 'CT_RUNDIR': '/run/user/1000'}
HP = {'SMOKE_CT_IMAGE': 'gap5-systemd', 'SMOKE_CT_USER': 'tester', 'SMOKE_CT_UID': '1001', 'SMOKE_CT_DOCKER': 'docker'}
HP_VALUES = {'CT_IMAGE': 'gap5-systemd', 'CT_USER': 'tester', 'CT_UID': '1001', 'CT_DOCKER': 'docker',
             'CT_HOME': '/home/tester', 'CT_RUNDIR': '/run/user/1001'}
NAMES = list(DEFAULTS)


def env(extra):
    e = {k: v for k, v in os.environ.items() if not k.startswith('SMOKE_CT_')}
    e.update(extra)
    return e


def shell(extra):
    out = subprocess.run(['bash', '-c', '. ct_env.sh; ' + '; '.join(f'echo "{n}=${n}"' for n in NAMES)],
                         cwd=HERE, env=env(extra), capture_output=True, text=True)
    return out.returncode, dict(line.split('=', 1) for line in out.stdout.splitlines())


def python(extra):
    code = 'import ct_env; print(*[getattr(ct_env, n) for n in %r], sep="\\n")' % NAMES
    out = subprocess.run([sys.executable, '-I', '-c', 'import sys; sys.path.insert(0, %r); %s' % (str(HERE), code)],
                         env=env(extra), capture_output=True, text=True)
    return out.returncode, dict(zip(NAMES, out.stdout.splitlines()))


class CtEnv(unittest.TestCase):
    def test_defaults(self):
        for f in (shell, python):
            self.assertEqual(f({}), (0, DEFAULTS), f.__name__)

    def test_hp_values(self):
        for f in (shell, python):
            self.assertEqual(f(HP), (0, HP_VALUES), f.__name__)

    def test_bad_values_are_refused(self):
        for f in (shell, python):
            for bad in ({'SMOKE_CT_USER': 'a b;rm'}, {'SMOKE_CT_UID': 'x'}, {'SMOKE_CT_DOCKER': 'docker; id'}, {'SMOKE_CT_IMAGE': '-x'}):
                self.assertNotEqual(f(bad)[0], 0, (f.__name__, bad))

    def test_remote_preamble_carries_the_settings(self):
        out = subprocess.run(['bash', '-c', '. ct_env.sh; ct_env_remote | bash -c "cat >/dev/null"; ct_env_remote'],
                             cwd=HERE, env=env(HP), capture_output=True, text=True).stdout
        remote = subprocess.run(['bash', '-c', out + 'echo "$CT_DOCKER $CT_USER $CT_RUNDIR"'], env=env({}), capture_output=True, text=True).stdout
        self.assertEqual(remote.strip(), 'docker tester /run/user/1001')

    def test_uninstall_fresh_follows_the_settings(self):
        code = ('import sys; sys.path.insert(0, HERE); import uninstall_fresh as u; '
                'print(u.DOCKER, u.USER_EXEC, u.PB, u.REGISTRATION, sep="|"); '
                "print('runuser -u %s -- env XDG_RUNTIME_DIR=%s' % (u.CT_USER, u.CT_RUNDIR) in u.SCAN, '! -path %s/pb' % u.CT_HOME in u.SCAN)").replace('HERE', repr(str(HERE)))
        def run(extra):
            return subprocess.run([sys.executable, '-c', code], env=env(extra), capture_output=True, text=True, cwd='/').stdout.splitlines()
        self.assertEqual(run({}), ['sudo -n docker|-u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000|/home/pbt/.local/bin/pb|'
                                   '/home/pbt/.local/state/paperboat/runtime/machine-registration.json', 'True True'])
        self.assertEqual(run(HP), ['docker|-u tester -e HOME=/home/tester -e XDG_RUNTIME_DIR=/run/user/1001|/home/tester/.local/bin/pb|'
                                   '/home/tester/.local/state/paperboat/runtime/machine-registration.json', 'True True'])


# Files that may name the defaults: the helper itself, the image's own Dockerfile and this test. Comments are skipped.
ALLOWED = {'ct_env.sh', 'ct_env.py', 'test-systemd.Dockerfile', 'test_ct_env.py'}
FORBIDDEN = re.compile(r'-u pbt\b|/home/pbt|run/user/1000|test-systemd|gap5-systemd|sudo (-n )?docker')
# Python rows and docs not parametrised yet (their --pb/--*-cmd defaults and usage examples, host-side iptables helpers
# that name no container user). Shrink this set, never grow it.
PENDING = {
    'config_sync_pair.py',
    'container_cut.sh',
    'control_outage.py',
    'control_outage_mac.py',
    'control_outage_windows.py',
    'device_paths.py',
    'direct_path.py',
    'env_team_scopes.py',
    'env_undeliverable.py',
    'flood_rows.py',
    'ip_rules_rows.py',
    'machine_ctl.py',
    'machine_tags.py',
    'multi_account_host_windows.py',
    'origin_tunnel_matrix.py',
    'policy_team.py',
    'policy_token.py',
    'preview_host_header.py',
    'rebind.py',
    'relay_paths.py',
    'route_rules_rows.py',
    'ssh_mac_rows.py',
    'team_two_account.py',
    'terminal_matrix.py',
    'transfer_modes_windows.py',
    'update_floor.py',
    'update_outage.py',
    'windows_checks.py',
    'winsandbox-v6only/winsandbox_v6only.sh',
    'xplat_fleet.py',
}


class NoNewHardcoding(unittest.TestCase):
    def test_tree_scan(self):
        found = []
        for path in sorted(HERE.rglob('*')):
            rel = path.relative_to(HERE).as_posix()
            if not path.is_file() or path.suffix not in ('.sh', '.py', '') or '__pycache__' in rel or rel in ALLOWED or rel in PENDING or rel.startswith('test_'):
                continue
            try:
                lines = path.read_text().splitlines()
            except UnicodeDecodeError:
                continue
            found += [f'{rel}:{n}: {l.strip()[:100]}' for n, l in enumerate(lines, 1)
                      if FORBIDDEN.search(l) and not l.lstrip().startswith('#')]
        self.assertEqual(found, [], 'hardcoded container settings (use ct_env.sh / ct_env.py):\n' + '\n'.join(found))


if __name__ == '__main__':
    unittest.main()
