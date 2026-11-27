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
Each returns (ok, detail) like the other fixture checks; the full output is kept in results/fixture-checks.
"""
import hashlib
import json
import os
from pathlib import Path
import re
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


CHECKS = {'console-team-admin': console_team_admin, 'console-signout-switch': console_signout_switch, 'console-invite-mail': console_invite_mail}
