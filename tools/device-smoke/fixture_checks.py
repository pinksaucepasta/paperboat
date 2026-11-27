#!/usr/bin/env python3
"""The checks that need operator fixtures (README.md "Fixtures"): free-plan, config-team-default, team-business, recording-key-rotation.

usage: fixture_checks.py free-plan|config-team-default|team-business|recording-key-rotation|fresh-install-window|billing-console|bandwidth-throttle-send|plus-alias-signup|billing-team-lifecycle
       fixture_checks.py console-team-admin|console-signout-switch|console-invite-mail|console-recording-replay   (console_rows.py)
       fixture_checks.py staging-edge-outage   (staging_edge_outage.py: STAGING only)
       fixture_checks.py staging-edge-idle     (staging_edge_idle.py: STAGING only, edges set to a 120 s idle limit first)
       fixture_checks.py policy-cli|policy-token|machine-tags|policy-team|tunnel-ip-rules|preview-ip-rules|tunnel-route-rules|tunnel-create|tunnel-origin|tunnel-origin-proxies   (access_checks.py)
Each check makes its own throwaway machines in Docker containers on SMOKE_FIXTURE_HOST (fixture_machine.sh: paired as the
account of a stored console session), runs the unchanged check script against them and revokes them on every exit path.
Prints one JSON receipt line {"check", "status": PASS|FAIL, "detail", "seconds"}; exit 0 only for PASS.
  free-plan            the Free fixture account (SMOKE_FREE_STATE): free_plan.py runs on its machine.
  config-team-default  the smoke team's owner (SMOKE_OPERATOR_STATE): config_team_default.py TEAM SMOKE_CONFIG_REPO on its machine.
  team-business        two machines of the owner and a per-run S3 store (versitygw, official image, filesystem backend, in the
                       machine's network namespace) with the keys of ~/.f19-s3ak and ~/.f19-s3sk; team_business.py runs from here.
  billing-console, bandwidth-throttle-send, plus-alias-signup, billing-team-lifecycle  billing_checks.py (F13-03S, F13-04, F13-04B, F13-04C, F13-06, F13-07)
  recording-key-rotation  a recording key only grows, so every run needs a team without one: two throwaway console accounts (owner,
                       member; fresh_account.mjs), a paid team the owner makes with `pb team create` and pays in Stripe's SANDBOX
                       Checkout with the 4242 test card (stripe_checkout.mjs), the member invited and accepting, two machines of the owner (one runs recordings_key_rotation.py, the other is its team
                       target) and the member's pb as a profile in the first machine's container. The console start (the only place
                       a recording starts) runs on this controller, which holds node, Chromium and the owner's session; the
                       container asks for it through files (console_start_bridge.py). Everything is removed at the end (the member leaves; deleting the owner's account deletes the team and cancels its subscription).
  fresh-install-window (AU4)  after release SMOKE_RELEASE_NEW is Latest: a machine of the owner (SMOKE_OPERATOR_STATE) on SMOKE_AU4_HOST
                       installs release SMOKE_RELEASE_OLD's official asset in a zone where today's 04:00 window passed, and keeps it
                       for SMOKE_AU4_OBSERVE s (default 1800).
preflight_fixtures.py verifies the fixtures first (smoke.sh and windows_checks.py run it); setup is never retried here.
"""
import contextlib
import json
import os
from pathlib import Path
import secrets
import shlex
import shutil
import subprocess
import sys
import tempfile
import threading
import time

import console_rows
import staging_edge_idle
import staging_edge_outage
import ct_env
import dev_targets
from results_dir import results_dir

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
        self.leftovers = []  # what a cleanup step could not remove: reported with the result, never hidden
        self.env = dict(os.environ, ORIGIN=ENV['SMOKE_CONSOLE_ORIGIN'], PW_CHROMIUM=path('SMOKE_PW_CHROMIUM'), SMOKE_CT_DOCKER=DOCKER,
                        **({'PW_MODULE': path('SMOKE_PW_MODULE')} if ENV.get('SMOKE_PW_MODULE') else {}),
                        **{k: ENV[k] for k in ('SMOKE_BROWSER_HOST', 'SMOKE_BROWSER_BIN') if ENV.get(k)})  # remote_chrome.sh

    def machine(self, label, state_key=None, state=None, binary=None, zone=None, host=None, name=None):
        """(name, machine id, prefix) of a machine paired as the account of the stored session (a setting, or a state file path);
        prefix runs a command there (stdin passes). binary: the build to install (default SMOKE_FIXTURE_PB); zone: its local time zone."""
        host, name = host or HOST, name or 'fx-%s-%s' % (label, self.run_id)
        env = dict(self.env, **({'SMOKE_CT_TZ': zone} if zone else {}))
        done = run([str(HERE / 'fixture_machine.sh'), 'up', host, name, binary or path('SMOKE_FIXTURE_PB'), state or path(state_key)], timeout=900, check=False, env=env)
        if done.returncode:
            self.callback(self._down, host, name)
            raise RuntimeError('fixture machine %s: %s' % (name, done.stderr.strip()[-300:]))
        made = json.loads(done.stdout.strip().splitlines()[-1])
        self.callback(self._down, host, name, made['machine_id'])
        return name, made['machine_id'], made['prefix'].replace(' exec ', ' exec -i ', 1)

    def _down(self, host, name, *machine_id):
        done = run([str(HERE / 'fixture_machine.sh'), 'down', host, name, *machine_id], check=False, env=self.env, timeout=300)
        if done.returncode:
            self.leftovers.append('fixture machine %s NOT revoked: %s' % (name, done.stderr.strip()[-150:]))

    def at(self, prefix, command, **kw):
        return run(shlex.split(prefix) + ['bash -lc ' + shlex.quote(command)], **kw)

    def node(self, script, *args, state=None, check=True, timeout=600):
        return run(['node', str(HERE / script), *args], env=dict(self.env, **({'STATE': state} if state else {})), check=check, timeout=timeout)

    def account(self, label):
        """(email, console session state file) of a new throwaway account (fresh_account.mjs); deleted, with its login files, when the block ends.
        The sign-up code mail sometimes never arrives: one more account is tried then (the abandoned one has no session to delete)."""
        for attempt in (1, 2):
            directory = str(Path(tempfile.gettempdir()) / ('fx-%s-%s-%d' % (label, self.run_id, attempt)))
            try:
                self.node('fresh_account.mjs', 'signup', directory, state=directory + '/state.json')
            except RuntimeError:
                shutil.rmtree(directory, ignore_errors=True)
                if attempt == 2:
                    raise
                continue
            self.callback(self._delete_account, directory)  # registered before anything uses the account, so it runs after all of it
            return json.loads(Path(directory, 'account.json').read_text())['email'], directory + '/state.json'

    def _delete_account(self, directory):
        done = self.node('fresh_account.mjs', 'delete', directory, state=directory + '/state.json', check=False)
        if done.returncode:
            self.leftovers.append('throwaway account NOT deleted, its login stays in %s: %s' % (directory, (done.stdout + done.stderr)[-150:].strip()))
        else:
            shutil.rmtree(directory, ignore_errors=True)


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
    state = run(shlex.split(prefix) + ['bash -lc ' + shlex.quote('%s status 2>&1 | grep -E "Daemon|Runtime|Path|Health" | head -8' % PB)], check=False, timeout=60).stdout
    raise RuntimeError('the fresh machine %s never became usable within %d s: %s [pb status: %s]' % (name, seconds, last, ' '.join(state.split())))


def on_machine(prefix, script, args, timeout):
    """Run a staged script on the machine; returns (exit code, output)."""
    done = run(shlex.split(prefix) + ['bash -lc ' + shlex.quote('cd d && PATH=%s/.local/bin:$PATH timeout %d python3 %s %s' % (HOME, timeout, script, ' '.join(map(shlex.quote, args))))],
               check=False, timeout=timeout + 60)
    return done.returncode, done.stdout + done.stderr


def keep(check, text):
    """The check's full output in the results directory (mode 600); returns its path for the receipt."""
    folder = results_dir(__file__) / 'fixture-checks'
    folder.mkdir(parents=True, exist_ok=True)
    path = folder / ('%s-%s.log' % (check, time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())))
    path.write_text(text)
    path.chmod(0o600)
    return str(path)


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
    return ok, 'state=%s allowance=%s usage=%s' % (record.get('state'), record.get('allowance_bytes'), record.get('usage_bytes')) if ok else ' '.join(out.split())[-300:] + ' log ' + keep('free-plan', out)


def config_team_default(m):
    repo = ENV['SMOKE_CONFIG_REPO']
    name, _, prefix = m.machine('cfg', 'SMOKE_OPERATOR_STATE')
    stage(prefix)
    ready(prefix, name)
    code, out = on_machine(prefix, 'config_team_default.py', [TEAM, repo], 900)
    lines = [l for l in out.splitlines() if l.startswith(('PASS ', 'FAIL '))]
    ok = code == 0 and len(lines) == 3 and all(l.startswith('PASS ') for l in lines)
    return ok, '; '.join(l[5:60] for l in lines) if ok else ' '.join(out.split())[-300:] + ' log ' + keep('config-team-default', out)


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
    return ok, (result[-1] if ok else '; '.join(f[:120] for f in fails[:4] or [' '.join(out.split())[-250:]]) + ' log ' + keep('team-business', out))


class ConsoleStart(threading.Thread):
    """Answers the container's console-start requests (console_start_bridge.py) with console_record_start.mjs, run here."""

    def __init__(self, m, prefix, state):
        super().__init__(daemon=True)
        self.m, self.prefix, self.state, self.done, self.refusals = m, prefix, state, threading.Event(), []

    def run(self):
        while not self.done.wait(1.5):
            try:
                found = self.m.at(self.prefix, 'python3 d/console_start_bridge.py claim', check=False, timeout=60)
                for line in found.stdout.splitlines():
                    self.answer(json.loads(line))
            except (RuntimeError, ValueError, subprocess.TimeoutExpired):
                continue  # one lost poll; the container's own request times out if this never recovers

    def answer(self, request):
        try:
            done = self.m.node('console_record_start.mjs', '--team', request['team'], '--session', request['session'], state=self.state, check=False, timeout=240)
            code, message = done.returncode, (done.stderr or done.stdout).strip()
        except (RuntimeError, subprocess.TimeoutExpired) as error:
            code, message = 1, str(error)
        if code:  # the check itself only sees "the console could not start the recording": keep the reason for the result
            self.refusals.append(message[:200])
            print('console start refused: ' + message[:300], file=sys.stderr, flush=True)
        self.m.at(self.prefix, 'python3 d/console_start_bridge.py answer %s %d' % (request['id'], code), stdin=message, timeout=60)

    def close(self):
        self.done.set()
        self.join(timeout=300)


def secret_file(m, prefix, name):
    """A new random password in ~/name of the machine (mode 600); returned for the caller to pipe into `pb env init`, never printed."""
    password = secrets.token_hex(12) + 'Aa1!'
    m.at(prefix, 'umask 077; cat > %s' % name, stdin=password)
    return password


def member_profile(m, prefix, state):
    """~/pbm in the machine: pb signed in as the member account (an API token of its console session) in a profile of its own."""
    token = m.node('mint_api_token.mjs', 'fx-recording-key', state=state).stdout
    m.at(prefix, 'cd d && python3 teammate_login.py', stdin=token)
    root = '%s/.local/state/paperboat/device-smoke/teammate' % HOME
    m.at(prefix, 'printf %%s %s > pbm && chmod 755 pbm' % shlex.quote(
        '#!/bin/sh\nexport HOME=%s/home XDG_CONFIG_HOME=%s/config XDG_DATA_HOME=%s/data XDG_STATE_HOME=%s/state XDG_CACHE_HOME=%s/cache XDG_RUNTIME_DIR=%s\n'
        'exec %s --config %s/config.json "$@"\n' % (root, root, root, root, root, ct_env.CT_RUNDIR, PB, root)))


# every member may use and replay every machine of the team (the owner and the member are viewers of its key). Not `record`: that rule makes the
# machine record every session by itself (trigger rule), and the check starts its recordings by hand (trigger manual)
TEAM_POLICY = {'rules': [{'src': ['*'], 'dst': ['machine:*'], 'allow': ['*', 'recording_view']}]}


def team_generation(m, prefix, team):
    shown = json.loads(m.at(prefix, '%s team get %s --json' % (PB, team)).stdout)
    return (shown.get('data') or shown)['generation']


def paid_team(m, prefix, member_email=None):
    """A new paid team of the owner whose pb runs at `prefix`; with member_email, the member (pb: ~/pbm there) is an active member of it. Returns its id.
    The product's own path: `pb team create` prints the Checkout link, the sandbox payment makes the team. The member leaves at the end;
    deleting the owner's account (the last cleanup step) then deletes the team and cancels its subscription."""
    team = 'pbkey-' + m.run_id
    created = json.loads(m.at(prefix, '%s team create %s --seats 2 --interval month --accept-terms --no-wait --json' % (PB, team)).stdout.strip().splitlines()[-1])
    m.node('stripe_checkout.mjs', created['checkout_url'], timeout=300)
    deadline = time.monotonic() + 180
    while not m.at(prefix, '%s team get %s --json' % (PB, team), check=False).returncode == 0:  # the team exists when Stripe has reported the payment
        if time.monotonic() > deadline:
            raise RuntimeError('team %s was not created within 180 s of the sandbox payment' % team)
        time.sleep(3)
    if not member_email:
        return team
    invitation = json.loads(m.at(prefix, '%s team invite %s %s --generation %s --json' % (PB, team, shlex.quote(member_email), team_generation(m, prefix, team))).stdout.strip().splitlines()[-1])
    m.at(prefix, '%s/pbm team accept %s --json' % (HOME, invitation['invitation_id']))
    m.callback(lambda: m.at(prefix, '%s/pbm team leave %s --generation %s --json' % (HOME, team, team_generation(m, prefix, team)), check=False))
    return team


def recording_key_rotation(m):
    owner, owner_state = m.account('owner')
    member, member_state = m.account('member')
    name, _, prefix = m.machine('own', state=owner_state)  # runs the check: its pb is the team owner
    target, target_id, _ = m.machine('tgt', state=owner_state)  # the team machine that records
    # shortcut: both machines exist before the first command runs on one of them: with a single machine of a new account `pb exec` on itself
    # answered "service is not ready yet" for 7 minutes (twice), with two it works; look at it again when exec on a lone machine is tested
    stage(prefix)
    ready(prefix, name)
    ready(prefix, target)  # the path the check uses: the owner's machine runs a command on the target
    member_profile(m, prefix, member_state)
    team = paid_team(m, prefix, member)
    secret = {who: secret_file(m, prefix, who + '.pw') for who in ('owner', 'member')}
    m.at(prefix, '%s env init --password-stdin' % PB, stdin=secret['owner'])
    m.at(prefix, '%s/pbm env init --password-stdin' % HOME, stdin=secret['member'])  # the key is sealed to every viewer who has a vault
    m.at(prefix, 'echo %s > policy.json && %s policy set policy.json --workspace %s --yes --message recording-key-rotation && rm policy.json'
         % (shlex.quote(json.dumps(TEAM_POLICY)), PB, team))
    m.at(prefix, '%s team machine transfer-to-team %s %s --confirm %s --generation %s --json' % (PB, team, target_id, target_id, team_generation(m, prefix, team)))
    m.callback(leave_team, m, prefix, team, target_id)
    bridge = ConsoleStart(m, prefix, owner_state)
    bridge.start()
    m.callback(bridge.close)
    code, out = on_machine(prefix, 'recordings_key_rotation.py', [target, '--team', team, '--vault-password-file', HOME + '/owner.pw', '--member-cmd', HOME + '/pbm',
                                                                  '--console-start-cmd', 'python3 console_start_bridge.py'], 2400)
    record = last_json(out)
    print(json.dumps(record), file=sys.stderr, flush=True)  # ids, generations and exit codes only
    bad = ['%s: %s' % (s['step'], s.get('error') or s.get('reason', '')) for s in record.get('steps', []) if s['status'] != 'PASS']
    ok = code == 0 and record.get('status') == 'PASS' and not bad
    if not ok:  # what the team's recordings look like now (ids, trigger, state: never their content), for the failure report
        listed = m.at(prefix, "%s recordings list --team %s --json | python3 -c 'import json,sys;d=json.load(sys.stdin);d=d.get(\"data\",d);"
                      "print([{k:r.get(k) for k in (\"id\",\"trigger\",\"state\",\"session_id\",\"machine_name\")} for r in d[\"recordings\"]])'" % (PB, team), check=False, timeout=90)
        print('recordings now: ' + (listed.stdout + listed.stderr).strip()[-600:], file=sys.stderr, flush=True)
    g = record.get('generations', {})
    return ok, ('replace kept old recordings readable, reset made them unreadable, new recordings work; generations %s -> %s -> %s' % (g.get('first'), g.get('replace'), g.get('reset'))
                if ok else '; '.join(bad[:3] + bridge.refusals[:1]) or ' '.join(out.split())[-300:])


def leave_team(m, prefix, team, machine_id):
    """The target leaves the team before its container revokes it (a team machine is removed from the team side first)."""
    try:
        m.at(prefix, '%s team machine remove %s %s --confirm %s --generation %s --json' % (PB, team, machine_id, machine_id, team_generation(m, prefix, team)), timeout=120)
    except (RuntimeError, subprocess.TimeoutExpired) as error:
        m.leftovers.append('team machine %s was NOT removed from the team: %s' % (machine_id, str(error)[-150:]))


REPOSITORY = 'pinksaucepasta/paperboat'
ZONES = ('Asia/Kolkata', 'Europe/Berlin', 'America/New_York', 'Asia/Tokyo', 'America/Los_Angeles', 'Pacific/Auckland', 'Europe/London')


def fetch(url, timeout=120):
    import urllib.request
    with urllib.request.urlopen(urllib.request.Request(url, headers={'User-Agent': 'paperboat-smoke'}), timeout=timeout) as response:
        return response.read()


def past_window_zone(now=None):
    """A zone where today's 04:00 update window passed two or more hours ago and the next one is two or more hours away."""
    import datetime
    import zoneinfo
    now = now or datetime.datetime.now(datetime.timezone.utc)
    for zone in ZONES:
        if 6 <= now.astimezone(zoneinfo.ZoneInfo(zone)).hour <= 21:
            return zone
    raise RuntimeError('no zone in %s has its local time between 06:00 and 21:59' % (ZONES,))


def fresh_install_window(m):
    """AU4: a machine installed with release A while B is already Latest, after today's 04:00 window in its own zone, keeps A
    until the next window (smoke 11-26: a fresh install updated itself to Latest ~30 s later). Precondition: B is Latest."""
    import hashlib
    old, new = os.environ['SMOKE_RELEASE_OLD'], os.environ['SMOKE_RELEASE_NEW']
    observe = int(os.environ.get('SMOKE_AU4_OBSERVE', '1800'))
    latest = json.loads(fetch('https://api.github.com/repos/%s/releases/latest' % REPOSITORY))['tag_name']
    if latest != new:
        return False, 'precondition: release %s must be Latest, Latest is %s' % (new, latest)
    host = os.environ.get('SMOKE_AU4_HOST') or HOST
    arch = {'x86_64': 'amd64', 'aarch64': 'arm64'}[run(['ssh', '-o', 'BatchMode=yes', host, 'uname -m']).stdout.strip()]
    asset = 'pb-linux-' + arch
    base = 'https://github.com/%s/releases/download/%s/' % (REPOSITORY, old)
    body = fetch(base + asset, timeout=600)
    sums = dict(reversed(line.split()) for line in fetch(base + 'SHA256SUMS').decode().splitlines() if line and not line.startswith('#'))
    if sums.get(asset) != hashlib.sha256(body).hexdigest():
        return False, '%s of %s does not match its SHA256SUMS' % (asset, old)
    folder = Path(tempfile.mkdtemp(prefix='au4-'))
    m.callback(shutil.rmtree, folder, ignore_errors=True)
    binary = folder / 'pb'
    binary.write_bytes(body)
    binary.chmod(0o755)
    zone = past_window_zone()
    name, _, prefix = m.machine('au4', 'SMOKE_OPERATOR_STATE', binary=str(binary), zone=zone, host=host, name=os.environ.get('SMOKE_AU4_NAME'))
    def pb(*args):
        done = m.at(prefix, '%s %s --json' % (PB, ' '.join(args)), timeout=180)
        value = json.loads(done.stdout)
        return value.get('data', value)
    local_hour = int(m.at(prefix, 'date +%H').stdout.strip())
    if not 6 <= local_hour <= 21:
        return False, 'the machine local time is not in %s (hour %d): the window would not have passed' % (zone, local_hour)
    installed = pb('--version')['version']
    if installed != old:
        return False, 'installed %s, not release %s' % (installed, old)
    import datetime
    import zoneinfo
    planned = datetime.datetime.fromisoformat(pb('update', 'status')['next_maintenance_at'].replace('Z', '+00:00')).astimezone(zoneinfo.ZoneInfo(zone))
    today = datetime.datetime.now(zoneinfo.ZoneInfo(zone)).date()
    if (planned.hour, planned.minute) != (4, 0) or planned.date() != today + datetime.timedelta(days=1):
        return False, 'pb does not plan the window at 04:00 tomorrow in %s (next maintenance %s): the zone did not take' % (zone, planned.isoformat())
    check = pb('update', 'check')
    if check.get('latest_version') != new or not check.get('update_available'):
        return False, 'the machine does not see %s as available: %r' % (new, {k: check.get(k) for k in ('latest_version', 'update_available')})
    began, samples = time.monotonic(), 0
    while time.monotonic() - began < observe:
        seen = pb('--version')['version']
        samples += 1
        if seen != old:
            return False, 'installed %s %d s after a fresh install of %s in %s (local hour %d), outside the window' % (seen, round(time.monotonic() - began), old, zone, local_hour)
        time.sleep(60)
    import update_health
    try:  # healthy, with every component of the fresh install on A (the guard included)
        health = update_health.settled(old, 300, doctor=lambda: json.loads(m.at(prefix, '%s doctor --json' % PB, timeout=180, check=False).stdout),
                                       status=lambda: pb('update', 'status'), version=lambda: pb('--version')['version'],
                                       run_shell=lambda script: m.at(prefix, script, timeout=180, check=False).stdout)
    except AssertionError as error:
        return False, str(error)
    status = pb('update', 'status')
    return True, 'fresh install of %s in %s (local hour %d, window 04:00 passed) stayed on it for %d s (%d samples) with %s Latest; next maintenance %s' % (
        old, zone, local_hour, observe, samples, new, status.get('next_maintenance_at')) + '; healthy, components on it: %s' % ', '.join(health['components'])


CHECKS = {'free-plan': free_plan, 'config-team-default': config_team_default, 'team-business': team_business, 'recording-key-rotation': recording_key_rotation,
          'fresh-install-window': fresh_install_window, 'staging-edge-outage': staging_edge_outage.check,
          'staging-edge-idle': staging_edge_idle.check, **console_rows.CHECKS}


def main(argv):
    import access_checks  # these two modules import this one, so only here
    import billing_checks
    CHECKS.update(billing_checks.CHECKS)
    CHECKS.update(access_checks.CHECKS)
    if len(argv) != 2 or argv[1] not in CHECKS:
        print(__doc__, file=sys.stderr)
        return 2
    began = time.monotonic()
    try:
        with Machines() as m:
            try:
                ok, detail = CHECKS[argv[1]](m)
            except Exception as error:  # noqa: BLE001 the fixture failed: a FAIL with the reason, never a silent skip
                ok, detail = False, '%s: %s' % (type(error).__name__, error)
        if m.leftovers:  # a record that stays behind is a harness failure, whatever the check said
            ok, detail = False, '%s; LEFT BEHIND: %s' % (detail[:200], '; '.join(m.leftovers))
    except Exception as error:  # noqa: BLE001 a cleanup step raised
        ok, detail = False, '%s: %s' % (type(error).__name__, error)
    print(json.dumps({'check': argv[1], 'status': 'PASS' if ok else 'FAIL', 'detail': detail[:400], 'seconds': round(time.monotonic() - began)}), flush=True)
    return 0 if ok else 1


if __name__ == '__main__':
    sys.modules.setdefault('fixture_checks', sys.modules[__name__])
    sys.exit(main(sys.argv))
