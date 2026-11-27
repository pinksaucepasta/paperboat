"""Console browser rows of fixture_checks.py (registered there; README.md "Console rows"). One headless browser at a time.

  console-team-admin    F19-13: the paste-in checks check-f19.js (roles, audit log filters, CSV export), check-f19-security.js
                        (Team Security and Directory against the server) and check-audit-streams.js (Activity streaming) on
                        the smoke team as its owner (SMOKE_OPERATOR_STATE, only read). All three always run.
  console-signout-switch  F2-07: a session signed in for this run only (console_session.mjs, the owner's password file):
                        check-machines-paging.js, then console_signout_switch.mjs (the real table past one page when the account
                        has over 50 machines, switch Personal <-> team, sign out, old cookies refused, the shared operator
                        session still works).
  console-invite-mail   F12-02-mail-live: console_invite_mail.mjs invites a disposable inbox of this run to the smoke team,
                        reads the mail, resends from the console, opens the link signed out and signs up; the invitation is
                        cancelled by its id and the throwaway account deleted.
  console-recording-replay  F25-05: the recording-key-rotation fixture without the member (throwaway owner, team paid in
                        Stripe's sandbox, two machines on SMOKE_FIXTURE_HOST); record_for_replay.py makes three recordings on
                        the team machine and console/scripts/check-recordings.mjs replays them in the browser, ending with the
                        replay rules removed while the long one downloads. All of it is removed with the account.
Each returns (ok, detail) like the other fixture checks; the full output is kept in results/fixture-checks.
"""
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import tempfile

HERE = Path(__file__).resolve().parent
SCRIPTS = HERE.parents[1] / 'console' / 'scripts'
STEP = re.compile(r'^(PASS|FAIL|SKIP) {2}(.*)$')
ALLOWED_SKIPS = ('offline machine state',)  # not part of a row's cell; every other skipped step is a failure
EMAIL = re.compile(r'[\w.+-]+@[\w-]+(?:\.[\w-]+)+')

ADMIN_CHECKS = (('check-f19.js', '/team'), ('check-f19-security.js', '/team'), ('check-audit-streams.js', '/activity'))
PAGING_CHECKS = (('check-machines-paging.js', '/'),)


def fx():
    import fixture_checks  # the runner (settings, result log); imported late, it imports this module
    return fixture_checks


def redact(text):
    return EMAIL.sub('<email>', text)


def steps(label, code, output):
    """(passed, [failed step lines]) of one browser script. A script that printed no step, or exited nonzero without a
    failed step, is itself a failure: silence never passes."""
    seen = [m.groups() for m in (STEP.match(line) for line in output.splitlines()) if m]
    passed = sum(1 for state, _ in seen if state == 'PASS')
    failed = ['%s: %s' % (label, text) for state, text in seen if state == 'FAIL' or (state == 'SKIP' and not text.startswith(ALLOWED_SKIPS))]
    if not failed and (code or not passed):
        failed = ['%s: exit %d with %d passed steps: %s' % (label, code, passed, ' '.join(output.split())[-200:])]
    return passed, failed


def verdict(check, results):
    """(ok, detail) of [(label, exit code, output)]: every script's failures, never only the first one's."""
    counts, failed, log = [], [], []
    for label, code, output in results:
        passed, bad = steps(label, code, output)
        counts.append('%s %d/%d' % (label, passed, passed + len(bad)))
        failed += bad
        log.append('== %s (exit %d)\n%s' % (label, code, output))
    kept = fx().keep(check, redact('\n'.join(log)))  # the step list is the row's evidence, on PASS too
    if not failed:
        return bool(results), ', '.join(counts) or 'nothing ran'
    return False, redact('%s; FAIL %s; log %s' % (', '.join(counts), ' | '.join(f[:110] for f in failed[:4]), kept))


def script(m, label, name, *args, state, env=None, timeout=420):
    """(label, exit code, output) of one node script of this directory with STATE=state; a hang is a result too."""
    import subprocess
    try:
        done = subprocess.run(['node', str(HERE / name), *args], env=dict(m.env, STATE=state, **(env or {})),
                              capture_output=True, text=True, timeout=timeout, stdin=subprocess.DEVNULL)
    except subprocess.TimeoutExpired as error:
        partial = error.stdout.decode(errors='replace') if isinstance(error.stdout, bytes) else error.stdout or ''
        return label, 124, partial + '\nFAIL  timed out after %d s' % timeout
    return label, done.returncode, done.stdout + done.stderr


def paste_check(m, name, path, state, team):
    return script(m, name[:-3], '../../console/scripts/run-console-check.mjs', str(SCRIPTS / name), path, json.dumps({'__team': team}), state=state)


def digest(file):
    return hashlib.sha256(Path(file).read_bytes()).hexdigest()


def private_directory(m, prefix):
    directory = tempfile.mkdtemp(prefix=prefix)  # mode 700
    m.callback(shutil.rmtree, directory, ignore_errors=True)
    return directory


def console_team_admin(m):
    state, team = fx().path('SMOKE_OPERATOR_STATE'), fx().TEAM
    return verdict('console-team-admin', [paste_check(m, name, page, state, team) for name, page in ADMIN_CHECKS])


def operator_password_file():
    return os.path.expanduser(fx().ENV.get('SMOKE_OPERATOR_PASSWORD_FILE', '~/.config/paperboat-dev/console-workos.password'))


def own_session(m, directory):
    """A new console session of the smoke team's owner in `directory`, signed out again when the block ends."""
    me = m.node('console_api.mjs', 'GET', '/v1/me', state=fx().path('SMOKE_OPERATOR_STATE'))
    reply = json.loads(me.stdout.strip().splitlines()[-1])
    if reply['status'] != 200:
        raise RuntimeError('the shared operator session does not answer /v1/me: HTTP %s' % reply['status'])
    state = directory + '/state.json'
    m.node('console_session.mjs', 'password', reply['body']['data']['email'], operator_password_file(), state)
    original = directory + '/signed-in.json'
    shutil.copyfile(state, original)
    os.chmod(original, 0o600)
    m.callback(end_session, m, original)  # after the directory's removal is registered: runs before it
    return state


def end_session(m, state):
    done = script(m, 'end', 'console_signout_switch.mjs', 'end', state=state, env={'OPERATOR_STATE': fx().path('SMOKE_OPERATOR_STATE')}, timeout=240)
    if done[1]:
        m.leftovers.append("this run's own console session was NOT signed out: " + redact(' '.join(done[2].split()))[-150:])


def console_signout_switch(m):
    shared, team = fx().path('SMOKE_OPERATOR_STATE'), fx().TEAM
    before = digest(shared)
    state = own_session(m, private_directory(m, 'console-signout-'))
    results = [paste_check(m, name, page, state, team) for name, page in PAGING_CHECKS]
    results.append(script(m, 'signout-switch', 'console_signout_switch.mjs', 'run', team, state=state, env={'OPERATOR_STATE': shared}, timeout=600))
    unchanged = digest(shared) == before
    results.append(('operator-state-file', 0 if unchanged else 1, '%s  the shared operator session file is unchanged' % ('PASS' if unchanged else 'FAIL')))
    return verdict('console-signout-switch', results)


def mail_console_origin():
    """The console the server writes into mail links (its configured console origin); the one under test unless set."""
    return fx().ENV.get('SMOKE_MAIL_CONSOLE_ORIGIN') or fx().ENV['SMOKE_CONSOLE_ORIGIN']


def console_invite_mail(m):
    state, team = fx().path('SMOKE_OPERATOR_STATE'), fx().TEAM
    directory = private_directory(m, 'console-invite-')
    m.callback(remove_invitee, m, directory, state, team)  # runs before the directory goes
    return verdict('console-invite-mail', [script(m, 'invite-mail', 'console_invite_mail.mjs', 'run', team, directory, state=state,
                                                  env={'MAIL_ORIGIN': mail_console_origin()}, timeout=1500)])


def remove_invitee(m, directory, state, team):
    """The run's pending invitation is cancelled by id; its account, if the sign-up got that far, is deleted."""
    done = script(m, 'cleanup', 'console_invite_mail.mjs', 'cleanup', team, directory, state=state, timeout=300)
    if done[1]:
        m.leftovers.append('invitation of this run NOT cancelled: ' + redact(' '.join(done[2].split()))[-150:])
    if Path(directory, 'state.json').is_file():
        deleted = script(m, 'delete', 'fresh_account.mjs', 'delete', directory, state=directory + '/state.json',
                         env={'ORIGIN': mail_console_origin()}, timeout=900)  # its session is that console's
        if deleted[1]:
            kept = directory + '-kept'
            shutil.copytree(directory, kept)
            m.leftovers.append('throwaway account NOT deleted, its login stays in %s: %s' % (kept, redact(deleted[2][-150:].strip())))


# two rules on purpose: check-recordings.mjs removes every replay rule, and the owner keeps the rule that gives access
REPLAY_POLICY = {'rules': [{'src': ['*'], 'dst': ['machine:*'], 'allow': ['*']}, {'src': ['*'], 'dst': ['machine:*'], 'allow': ['recording_view']}]}


def replay_check(m, state, team, machine_id, recordings, password):
    """check-recordings.mjs against the three recordings; one that was not made fails by name (its steps would skip)."""
    secret = os.path.join(private_directory(m, 'console-replay-'), 'vault.pw')
    with open(os.open(secret, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'w') as handle:
        handle.write(password)
    env = {'CHROME': fx().path('SMOKE_PW_CHROMIUM'), 'CONSOLE_ORIGIN': fx().ENV['SMOKE_CONSOLE_ORIGIN'], 'TEAM': team, 'MACHINE': machine_id,
           'VAULT_PASSWORD_FILE': secret}
    env.update({variable: recordings[name] for name, variable in (('normal', 'RECORDING'), ('tiny', 'TINY_RECORDING'), ('long', 'LONG_RECORDING'))
                if recordings.get(name)})
    results = [script(m, 'check-recordings', '../../console/scripts/check-recordings.mjs', state=state, env=env, timeout=1200)]
    missing = [name for name in ('normal', 'tiny', 'long') if not recordings.get(name)]
    if missing:
        results.append(('recordings', 1, 'FAIL  not made: %s' % ', '.join(missing)))
    return results


def console_recording_replay(m):
    f = fx()
    _, owner_state = m.account('owner')
    name, _, prefix = m.machine('own', state=owner_state)  # runs pb as the team owner
    target, target_id, _ = m.machine('tgt', state=owner_state)  # the team machine that records and serves the replay
    f.stage(prefix)
    f.ready(prefix, name)
    f.ready(prefix, target)
    team = f.paid_team(m, prefix)
    password = f.secret_file(m, prefix, 'owner.pw')
    m.at(prefix, '%s env init --password-stdin' % f.PB, stdin=password)
    m.at(prefix, 'echo %s > policy.json && %s policy set policy.json --workspace %s --yes --message console-recording-replay && rm policy.json'
         % (shlex.quote(json.dumps(REPLAY_POLICY)), f.PB, team))
    m.at(prefix, '%s team machine transfer-to-team %s %s --confirm %s --generation %s --json' % (f.PB, team, target_id, target_id, f.team_generation(m, prefix, team)))
    m.callback(f.leave_team, m, prefix, team, target_id)
    bridge = f.ConsoleStart(m, prefix, owner_state)
    bridge.start()
    m.callback(bridge.close)
    code, out = f.on_machine(prefix, 'record_for_replay.py', [target, '--team', team, '--vault-password-file', f.HOME + '/owner.pw',
                                                              '--console-start-cmd', 'python3 console_start_bridge.py'], 1500)
    bridge.close()  # its browser is gone before the check's starts: one at a time
    made = f.last_json(out)
    results = replay_check(m, owner_state, team, target_id, made.get('recordings', {}), password)
    if code or made.get('status') != 'PASS':
        results.append(('record-for-replay', 1, 'FAIL  %s' % ('; '.join(made.get('errors', []) + bridge.refusals[:1]) or ' '.join(out.split())[-300:])))
    return verdict('console-recording-replay', results)


CHECKS = {'console-team-admin': console_team_admin, 'console-signout-switch': console_signout_switch, 'console-invite-mail': console_invite_mail,
          'console-recording-replay': console_recording_replay}
