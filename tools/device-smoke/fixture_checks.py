#!/usr/bin/env python3
"""The checks that need operator fixtures (README.md "Fixtures"): free-plan, config-team-default, team-business.

usage: fixture_checks.py free-plan|config-team-default|team-business
Each check makes its own throwaway machines in Docker containers on SMOKE_FIXTURE_HOST (fixture_machine.sh: paired as the
account of a stored console session), runs the unchanged check script against them and revokes them on every exit path.
Prints one JSON receipt line {"check", "status": PASS|FAIL, "detail", "seconds"}; exit 0 only for PASS.
  free-plan            the Free fixture account (SMOKE_FREE_STATE): free_plan.py runs on its machine.
  config-team-default  the smoke team's owner (SMOKE_OPERATOR_STATE): config_team_default.py TEAM SMOKE_CONFIG_REPO on its machine.
  team-business        two machines of the owner and a per-run S3 store (versitygw, official image, filesystem backend, in the
                       machine's network namespace) with the keys of ~/.f19-s3ak and ~/.f19-s3sk; team_business.py runs from here.
preflight_fixtures.py verifies the fixtures first (smoke.sh and windows_checks.py run it); setup is never retried here.
"""
import contextlib
import json
import os
from pathlib import Path
import secrets
import shlex
import subprocess
import sys
import time

import ct_env
import dev_targets

HERE = Path(__file__).resolve().parent
ENV = dev_targets.smoke_environment(dev_targets.load())
HOST = ENV.get('SMOKE_FIXTURE_HOST', '')
DOCKER, HOME = ct_env.CT_DOCKER, ct_env.CT_HOME
PB = HOME + '/.local/bin/pb'
S3_IMAGE = ENV.get('SMOKE_F19_S3_IMAGE', 'ghcr.io/versity/versitygw:latest')
TEAM = ENV.get('PAPERBOAT_SMOKE_TEAM', 'paperboat-smoke-paid')


def path(key):
    return os.path.expanduser(ENV[key])


def run(argv, timeout=120, stdin=None, check=True, env=None):
    done = subprocess.run(argv, capture_output=True, text=True, timeout=timeout, input=stdin, env=env, stdin=None if stdin is not None else subprocess.DEVNULL)
    if check and done.returncode:
        raise RuntimeError('%s failed (%d): %s' % (argv[0], done.returncode, (done.stdout + done.stderr)[-300:]))
    return done


def remote(command, **kw):
    return run(['ssh', '-o', 'BatchMode=yes', HOST, command], **kw)


class Machines(contextlib.ExitStack):
    """Throwaway fixture machines of one run; every one is revoked and removed when the block ends."""

    def __init__(self):
        super().__init__()
        self.run_id = '%s-%s' % (time.strftime('%H%M%S', time.gmtime()), secrets.token_hex(2))
        self.env = dict(os.environ, ORIGIN=ENV['SMOKE_CONSOLE_ORIGIN'], PW_CHROMIUM=path('SMOKE_PW_CHROMIUM'), SMOKE_CT_DOCKER=DOCKER,
                        **({'PW_MODULE': path('SMOKE_PW_MODULE')} if ENV.get('SMOKE_PW_MODULE') else {}))

    def machine(self, label, state_key):
        """(name, machine id, prefix) of a machine paired as the account of the stored session; prefix runs a command there (stdin passes)."""
        name = 'fx-%s-%s' % (label, self.run_id)
        done = run([str(HERE / 'fixture_machine.sh'), 'up', HOST, name, path('SMOKE_FIXTURE_PB'), path(state_key)], timeout=900, check=False, env=self.env)
        if done.returncode:
            self.callback(run, [str(HERE / 'fixture_machine.sh'), 'down', HOST, name], check=False, env=self.env)
            raise RuntimeError('fixture machine %s: %s' % (name, done.stderr.strip()[-300:]))
        made = json.loads(done.stdout.strip().splitlines()[-1])
        self.callback(run, [str(HERE / 'fixture_machine.sh'), 'down', HOST, name, made['machine_id']], check=False, env=self.env, timeout=300)
        return name, made['machine_id'], made['prefix'].replace(' exec ', ' exec -i ', 1)

    def at(self, prefix, command, **kw):
        return run(shlex.split(prefix) + ['bash -lc ' + shlex.quote(command)], **kw)


def stage(prefix):
    """The harness scripts in ~/d of the machine."""
    packed = subprocess.run(['tar', '-C', str(HERE), '-cf', '-'] + sorted(p.name for p in HERE.glob('*.py') if not p.name.startswith('test_')),
                            capture_output=True, check=True).stdout
    subprocess.run(shlex.split(prefix) + ['bash -c ' + shlex.quote('rm -rf d && mkdir d && tar -xf - -C d')], input=packed, capture_output=True, check=True, timeout=120)


def ready(prefix, name, seconds=420):
    """The fresh machine can run a command on itself: its account sync (machine list, native path) is done."""
    deadline, last = time.monotonic() + seconds, ''
    while time.monotonic() < deadline:
        done = run(shlex.split(prefix) + ['%s exec %s -- true' % (PB, name)], check=False, timeout=90)
        if not done.returncode:
            return
        last = (done.stdout + done.stderr)[-200:]
        time.sleep(10)
    raise RuntimeError('the fresh machine %s never became usable within %d s: %s' % (name, seconds, last))


def on_machine(prefix, script, args, timeout):
    """Run a staged script on the machine; returns (exit code, output)."""
    done = run(shlex.split(prefix) + ['bash -lc ' + shlex.quote('cd d && PATH=%s/.local/bin:$PATH timeout %d python3 %s %s' % (HOME, timeout, script, ' '.join(map(shlex.quote, args))))],
               check=False, timeout=timeout + 60)
    return done.returncode, done.stdout + done.stderr


def last_json(output):
    for line in reversed(output.strip().splitlines()):
        try:
            record = json.loads(line)
        except ValueError:
            continue
        if isinstance(record, dict):
            return record
    return {}


def free_plan(m):
    name, _, prefix = m.machine('free', 'SMOKE_FREE_STATE')
    stage(prefix)
    ready(prefix, name)
    code, out = on_machine(prefix, 'free_plan.py', [name], 900)
    record = last_json(out)
    ok = code == 0 and record.get('status') == 'PASS'
    return ok, 'state=%s allowance=%s usage=%s' % (record.get('state'), record.get('allowance_bytes'), record.get('usage_bytes')) if ok else ' '.join(out.split())[-300:]


def config_team_default(m):
    repo = ENV['SMOKE_CONFIG_REPO']
    name, _, prefix = m.machine('cfg', 'SMOKE_OPERATOR_STATE')
    stage(prefix)
    ready(prefix, name)
    code, out = on_machine(prefix, 'config_team_default.py', [TEAM, repo], 900)
    lines = [l for l in out.splitlines() if l.startswith(('PASS ', 'FAIL '))]
    ok = code == 0 and len(lines) == 3 and all(l.startswith('PASS ') for l in lines)
    return ok, '; '.join(l[5:60] for l in lines) if ok else ' '.join(out.split())[-300:]


def s3_store(m, machine, prefix):
    """versitygw (official image, posix backend, bucket `audit`) inside the machine's network namespace: 127.0.0.1:7070 from the machine.
    Returns the prefix that runs a command in the store's container (the check lists the stored objects through it)."""
    sibling = machine + '-s3'
    keys = [Path.home() / '.f19-s3ak', Path.home() / '.f19-s3sk']
    envfile = 'ROOT_ACCESS_KEY_ID=%s\nROOT_SECRET_ACCESS_KEY=%s\nVGW_REGION=eu-central-1\n' % tuple(k.read_text().strip() for k in keys)
    remote('umask 077; cat > /tmp/%s.env' % sibling, stdin=envfile)
    m.callback(remote, 'rm -f /tmp/%s.env; %s rm -f %s >/dev/null 2>&1' % (sibling, DOCKER, sibling), check=False)
    remote('%s pull -q %s >/dev/null && %s run -d --name %s --network container:%s --env-file /tmp/%s.env --entrypoint sh %s -c '
           '"mkdir -p /tmp/vgw/audit && exec versitygw --port 127.0.0.1:7070 posix --nometa /tmp/vgw" >/dev/null && rm -f /tmp/%s.env'
           % (DOCKER, S3_IMAGE, DOCKER, sibling, machine, sibling, S3_IMAGE, sibling), timeout=300)
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:  # the store answers on the machine's loopback
        if not m.at(prefix, 'curl -s -o /dev/null http://127.0.0.1:7070/', check=False).returncode:
            break
        time.sleep(1)
    else:
        raise RuntimeError('the S3 store did not start: ' + remote('%s logs --tail 5 %s 2>&1' % (DOCKER, sibling), check=False).stdout[-200:])
    for key in ('.f19-s3ak', '.f19-s3sk'):  # the check reads them by path on the machine
        m.at(prefix, 'umask 077; cat > %s' % key, stdin=(Path.home() / key).read_text().strip())
    return '%s exec -i %s' % (DOCKER, sibling)


def team_business(m):
    name, _, prefix = m.machine('biz', 'SMOKE_OPERATOR_STATE')
    peer, _, peer_prefix = m.machine('peer', 'SMOKE_OPERATOR_STATE')
    store = s3_store(m, name, prefix)
    ready(prefix, name)
    api = json.loads(m.at(prefix, '%s config show --json' % PB).stdout)
    api = (api.get('data') or api)['server_url']
    command = [sys.executable, str(HERE / 'team_business.py'), '--team', TEAM, '--cmd', prefix, '--api', api, '--pb', PB, '--alias', name,
               '--peer-alias', peer, '--peer-cmd', peer_prefix, '--s3-origin', '127.0.0.1:7070', '--s3-region', 'eu-central-1',
               '--s3-list-cmd', 'ssh %s %s' % (HOST, store)]
    done = run(command, check=False, timeout=3300)
    out = done.stdout + done.stderr
    fails = [l for l in out.splitlines() if 'FAIL' in l]
    result = [l for l in out.splitlines() if l.startswith('RESULT')]
    ok = done.returncode == 0 and result and result[-1].split()[1] == 'PASS'
    return ok, (result[-1] if ok else '; '.join(fails[:3] or [' '.join(out.split())[-250:]]))


CHECKS = {'free-plan': free_plan, 'config-team-default': config_team_default, 'team-business': team_business}


def main(argv):
    if len(argv) != 2 or argv[1] not in CHECKS:
        print(__doc__, file=sys.stderr)
        return 2
    began = time.monotonic()
    try:
        with Machines() as m:
            ok, detail = CHECKS[argv[1]](m)
    except Exception as error:  # noqa: BLE001 the fixture failed: a FAIL with the reason, never a silent skip
        ok, detail = False, '%s: %s' % (type(error).__name__, error)
    print(json.dumps({'check': argv[1], 'status': 'PASS' if ok else 'FAIL', 'detail': detail[:400], 'seconds': round(time.monotonic() - began)}), flush=True)
    return 0 if ok else 1


if __name__ == '__main__':
    sys.exit(main(sys.argv))
