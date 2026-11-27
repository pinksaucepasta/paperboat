"""Console page rows of fixture_checks.py (registered there; README.md "Console page rows"). Each row signs up its own
throwaway account(s), pairs its own container machine(s) on SMOKE_FIXTURE_HOST, runs the console/scripts browser checks
against the live console and removes everything again. The browser may run on another host (console_browser.sh).

  console-public          F15-06, W-14, W-08, W-13, F14-09 (console half): read only, on the smoke team owner's session
  console-account         F2-02, F1-12, F1-01 (Linux), F2-07, F2-06 (keep awake), F18-09, F18-11, F9-06
  console-account-delete  F2-09, with a machine, preview, tunnel, ENV vault and API token
  console-previews        F7-06 (New preview dialog on an account's first and next preview, list, stop), F17-01..F17-06
  console-tunnels         F8-08, F8-16, W-10, W-11, preview and tunnel IP rules
  console-files           F6-06 (own machines): the Files screens
  console-team            F6-06 (team requests), F12-07, F12-04, F27-01, W-15, F10-08 (team), F2-09 (refusal)
  console-env             F10-06, F10-07, F10-08 (one person)
  console-config-sync     F11-05: in sync, waiting for approval, conflict, resolved
  console-terminal        F3-07 (Linux), F3-08 (People panel, guest with a code, teammate)
  console-terminal-fleet  F3-07 on macOS and Windows machines named by SMOKE_CONSOLE_TERMINAL_TARGETS
  console-site            S2-02 (legal pages), F2-11 (/auth/failed, /auth/deleted), F9-08 (own regions), F11-08 (GitHub)
  console-access-login    F9-07 (/access-login for a private preview)
  console-announcements   W-16 (operator announcements: ribbon, bell, dismiss, retract)
  admin-panel             S3-09: the operator admin panel, read only

Every script of a row runs even after one failed; the row's detail names each failed step."""
import json
import os
import re
import shlex
import subprocess
import time

import console_rows
import ct_env

HERE = console_rows.HERE
SCRIPTS = console_rows.SCRIPTS


def fx():
    return console_rows.fx()


def browser_env():
    """What the console/scripts/*.mjs files read, beside the ORIGIN/PW_CHROMIUM of the paste-in runner."""
    f = fx()
    return {'CONSOLE_ORIGIN': f.ENV['SMOKE_CONSOLE_ORIGIN'], 'CHROME': f.path('SMOKE_PW_CHROMIUM')}


def paste(m, name, page, state, globals_=None, label=None, timeout=420, secret_globals=None):
    """One console/scripts/check-*.js in a signed-in tab on `page`: (label, exit code, output)."""
    return console_rows.script(m, label or name[:-3], '../../console/scripts/run-console-check.mjs', str(SCRIPTS / name), page,
                               json.dumps(globals_ or {}), state=state, timeout=timeout,
                               env={'CHECK_GLOBALS': json.dumps(secret_globals)} if secret_globals else None)


def node_check(m, name, *args, state, env=None, label=None, timeout=600):
    """One console/scripts/check-*.mjs: (label, exit code, output)."""
    return console_rows.script(m, label or name.rsplit('.', 1)[0], '../../console/scripts/' + name, *args, state=state,
                               env=dict(browser_env(), **(env or {})), timeout=timeout)


def api(m, state, method, path, body=None):
    """(HTTP status, parsed body) of one console API call with the stored session."""
    done = m.node('console_api.mjs', method, path, *([json.dumps(body)] if body is not None else []), state=state, timeout=120)
    reply = json.loads(done.stdout.strip().splitlines()[-1])
    return reply['status'], reply['body']


def data(m, state, path):
    status, body = api(m, state, 'GET', path)
    if status != 200:
        raise RuntimeError('GET %s answered HTTP %s %s' % (path.split('?')[0], status, json.dumps(body)[:200]))
    return body['data']


def step(label, ok, text):
    """A step the row itself checks, in the shape console_rows.verdict reads."""
    return label, 0 if ok else 1, '%s  %s' % ('PASS' if ok else 'FAIL', text)


def online(m, state, machine_id, seconds=420):
    """The console shows the new machine online with a terminal-capable service (its first account sync is done)."""
    deadline, last = time.monotonic() + seconds, None
    while time.monotonic() < deadline:
        status, body = api(m, state, 'GET', '/v1/machines/' + machine_id)
        last = body.get('data') if status == 200 and isinstance(body, dict) else None
        if last and (last.get('online') or last.get('status') == 'online' or (last.get('availability') or {}).get('online')):
            return last
        time.sleep(10)
    raise RuntimeError('machine %s never read online within %d s: %s' % (machine_id, seconds, json.dumps(last)[:200]))


def machine(m, label, state):
    """(name, machine id, prefix) of a new fixture machine of `state`'s account, online in the console."""
    name, machine_id, prefix = m.machine(label, state=state)
    online(m, state, machine_id)
    return name, machine_id, prefix


def serve(m, prefix, port):
    """A web app on 127.0.0.1:port of the fixture machine that answers every path (404s are captured too)."""
    m.at(prefix, 'curl -fs -o /dev/null http://127.0.0.1:%d/ || { mkdir -p site && echo console-row >site/index.html && '
                 '(cd site && nohup python3 -m http.server %d --bind 127.0.0.1 >/dev/null 2>&1 &) ; }; '
                 'for i in $(seq 20); do curl -fs -o /dev/null http://127.0.0.1:%d/ && exit 0; sleep 0.5; done; exit 1' % (port, port, port))


def one_space_steps(label, code, output):
    """final_console_*.sh print "PASS name: detail" with one space; the verdict reads two."""
    lines = []
    for line in output.splitlines():
        if line[:5] in ('PASS ', 'FAIL ') and line[5:6] != ' ':
            line = line[:4] + '  ' + line[5:]
        lines.append(line)
    return label, code, '\n'.join(lines)


def shell(m, label, argv, state, env=None, timeout=900):
    try:
        done = subprocess.run(argv, env=dict(m.env, STATE=state, **(env or {})), capture_output=True, text=True, timeout=timeout,
                              stdin=subprocess.DEVNULL)
    except subprocess.TimeoutExpired:
        return label, 124, 'FAIL  timed out after %d s' % timeout
    return one_space_steps(label, done.returncode, done.stdout + done.stderr)


def inspector_refusals(m, owner_state, stranger_state, prefix):
    """F17-02 from the browser's side: only the owner gets an inspector credential for a live preview."""
    f = fx()
    made = json.loads(m.at(prefix, '%s preview 18088 --background --ttl 20m --json' % f.PB, timeout=180).stdout)
    preview = (made.get('data') or made)['id']
    try:
        # the request console/lib/inspector/channel.ts sends
        request = {'resource_kind': 'preview', 'resource_id': preview, 'route_id': preview, 'action': 'inspect', 'ttl_seconds': 300}
        empty = os.path.join(console_rows.private_directory(m, 'console-anon-'), 'state.json')
        with open(empty, 'w') as handle:
            json.dump({'cookies': [], 'origins': []}, handle)
        code = lambda body: ((body or {}).get('error') or {}).get('code', '') if isinstance(body, dict) else ''
        owner, owner_body = api(m, owner_state, 'POST', '/v1/inspector/credentials', request)
        deadline = time.monotonic() + 90
        while owner not in (200, 201) and time.monotonic() < deadline:  # a preview a few seconds old is still being routed
            time.sleep(5)
            owner, owner_body = api(m, owner_state, 'POST', '/v1/inspector/credentials', request)
        stranger, stranger_body = api(m, stranger_state, 'POST', '/v1/inspector/credentials', request)
        anonymous, _ = api(m, empty, 'POST', '/v1/inspector/credentials', request)
        return [step('inspector-owner', owner in (200, 201), 'the owner gets an inspector credential (HTTP %s %s)' % (owner, code(owner_body))),
                step('inspector-stranger', stranger in (403, 404), 'another account is refused (HTTP %s %s)' % (stranger, code(stranger_body))),
                step('inspector-signed-out', anonymous in (401, 403), 'a signed-out request is refused (HTTP %s)' % anonymous)]
    finally:
        m.at(prefix, '%s preview stop %s --json' % (f.PB, preview), check=False, timeout=120)


def stop_previews(m, state):
    """Whatever a failed preview check left running on the throwaway account."""
    for item in data(m, state, '/v1/previews?workspace=personal')['items']:
        if item['state'] in ('ready', 'provisioning', 'starting'):
            api(m, state, 'DELETE', '/v1/previews/' + item['id'])


def console_previews(m):
    f = fx()
    _, state = m.account('prev')
    name, _, prefix = machine(m, 'pv', state)
    serve(m, prefix, 18088)
    # The account's first preview and then another: the page of an account without previews is a different one.
    wanted = {'__port': '18088', '__machine': name}
    results = [paste(m, 'check-previews.js', '/previews', state, wanted, label='first-preview', timeout=600)]
    if results[0][1]:
        stop_previews(m, state)
    results.append(paste(m, 'check-previews.js', '/previews', state, wanted, label='next-preview', timeout=600))
    stop_previews(m, state)
    results.append(shell(m, 'inspector', [str(HERE / 'final_console_inspector.sh'), f.HOST, name], state,
                         env=dict(browser_env(), HOLD=f.ENV.get('SMOKE_INSPECTOR_HOLD', '300')), timeout=1500))
    _, stranger = m.account('prev-other')
    results += inspector_refusals(m, state, stranger, prefix)
    return console_rows.verdict('console-previews', results)


def console_public(m):
    """Read-only on the smoke team owner's session: Activity (F15-06), the layout (W-14), every page reading the real
    answers (W-08), the Requests menu (W-13, its three sources stubbed in the tab) and the update summary (F14-09)."""
    state = fx().path('SMOKE_OPERATOR_STATE')
    results = [paste(m, 'check-activity.js', '/activity', state)]
    results += [node_check(m, name, state=state) for name in ('check-layout.mjs', 'check-answers-readable.mjs', 'check-requests-menu.mjs')]
    results.append(console_rows.script(m, 'update-summary', 'console_updates.mjs', state=state, timeout=900))
    return console_rows.verdict('console-public', [with_verdict_line(r) for r in results])


def with_verdict_line(result):
    """check-layout and check-answers-readable end with "all passed" and print only failures: count that as one step."""
    label, code, output = result
    if not code and 'PASS  ' not in output and output.strip().endswith('all passed'):
        return label, code, output + '\nPASS  all passed'
    return result


def container(f, name, command, stdin=None, timeout=600, root=False, check=True):
    """Run a shell string inside the fixture container `name`; stdin passes, so a secret never sits in an argument list."""
    user = '' if root else ct_env.CT_EXEC
    remote = '%s exec -i %s -w %s %s bash -c %s' % (f.DOCKER, user, ct_env.CT_HOME if not root else '/', name, shlex.quote(command))
    return f.remote(remote, stdin=stdin, timeout=timeout, check=check)


def add_machine_live(m, state):
    """F1-12 live and F1-01 (Linux): the Add machine dialog's own command, run on a clean machine while the dialog waits.
    Returns (results, (name, machine id) or None)."""
    f = fx()
    name = 'fx-add-%s' % m.run_id
    tool = str(HERE / 'fixture_machine.sh')
    m.callback(f.run, [tool, 'down', f.HOST, name], check=False, env=m.env, timeout=300)
    f.run([tool, 'bare', f.HOST, name], env=m.env, timeout=900)
    directory = console_rows.private_directory(m, 'console-add-')
    command_file = directory + '/command'
    output = directory + '/dialog.out'
    with open(output, 'w') as sink:
        dialog = subprocess.Popen(['node', str(HERE / 'console_add_machine.mjs'), str(SCRIPTS / 'check-add-machine.js')],
                                  env=dict(m.env, STATE=state, COMMAND_FILE=command_file), stdout=sink, stderr=subprocess.STDOUT,
                                  stdin=subprocess.DEVNULL)
    results, made = [], None
    try:
        deadline = time.monotonic() + 300
        while not os.path.exists(command_file) and dialog.poll() is None and time.monotonic() < deadline:
            time.sleep(1)
        if os.path.exists(command_file):
            with open(command_file) as handle:
                command = handle.read()
            install = container(f, name, 'export PATH=$HOME/.local/bin:$PATH; sh -s 2>&1', stdin=command.strip() + '\n', timeout=600, check=False)
            tail = install.stdout + install.stderr
            for token in re.findall(r"""PB_TOKEN=["']?([^"'\s;]+)""", command):
                tail = tail.replace(token, '<token>')
            tail = ' '.join(tail.split())[-300:]
            results.append(step('one-liner', install.returncode == 0, 'the dialog\'s one-liner installs and pairs on a clean Linux machine (exit %d)%s'
                                % (install.returncode, '' if not install.returncode else ': ' + tail)))
        else:
            results.append(step('one-liner', False, 'the dialog never gave its command'))
        try:
            dialog.wait(timeout=660)
        except subprocess.TimeoutExpired:
            dialog.kill()
    finally:
        if dialog.poll() is None:
            dialog.kill()
    with open(output) as handle:
        results.insert(0, ('add-machine-dialog', dialog.returncode or 0, handle.read()))
    found = container(f, name, 'cat .local/state/paperboat/runtime/machine-registration.json 2>/dev/null; true', check=False).stdout
    try:
        machine_id = json.loads(found)['machine_id']
        made = (name, machine_id)
        # the container revokes its own record before it goes, if the lifecycle check has not removed it
        m.callback(f.run, [tool, 'down', f.HOST, name, machine_id], check=False, env=m.env, timeout=300)
        installed = container(f, name, '.local/bin/pb version', check=False).stdout
        wanted = expected_release()
        results.append(step('one-liner-version', wanted in installed.split(), 'the one-liner installed release %s: %s' % (wanted, ' '.join(installed.split()[-3:]))))
    except (ValueError, KeyError):
        results.append(step('one-liner-registered', False, 'the clean machine has no registration after the one-liner'))
    return results, made


def expected_release():
    """The release the console's one-liner must install: SMOKE_FINAL_RELEASE, else the repository's latest release."""
    named = os.environ.get('SMOKE_FINAL_RELEASE') or fx().ENV.get('SMOKE_FINAL_RELEASE')
    if named:
        return named
    done = subprocess.run(['curl', '-sS', '-L', '-m', '60', '-o', '/dev/null', '-w', '%{url_effective}',
                           'https://github.com/pinksaucepasta/paperboat/releases/latest'], capture_output=True, text=True, timeout=90)
    return done.stdout.rstrip('/').rsplit('/', 1)[-1]


def cli_approval(m, state, prefix):
    """F2-02: `pb auth login --reauth` prints a link; the console's approval page approves it and the command succeeds."""
    f = fx()
    m.at(prefix, 'rm -f login.out login.rc; (timeout 900 %s auth login --reauth --no-browser --json >login.out 2>&1; echo $? >login.rc) >/dev/null 2>&1 &' % f.PB)
    url, deadline = '', time.monotonic() + 90
    while not url and time.monotonic() < deadline:
        text = m.at(prefix, 'cat login.out 2>/dev/null; true', check=False).stdout
        url = next((json.loads(line).get('approval_url') or (json.loads(line).get('data') or {}).get('approval_url', '')
                    for line in text.splitlines() if line.startswith('{') and 'approval_url' in line and line.endswith('}')), '')
        if not url:
            time.sleep(3)
    if not url:
        return [step('cli-approval', False, 'pb auth login --reauth printed no approval link: ' + ' '.join(text.split())[-200:])]
    approved = console_rows.script(m, 'cli-approval', 'approve_cli_login.mjs', url, state=state, timeout=300)
    results = [step('cli-approval-page', approved[1] == 0, 'the console approval page approves the printed link%s'
                    % ('' if not approved[1] else ': ' + ' '.join(approved[2].split())[-200:]))]
    code, deadline = '', time.monotonic() + 180
    while not code and time.monotonic() < deadline:
        code = m.at(prefix, 'cat login.rc 2>/dev/null; true', check=False).stdout.strip()
        time.sleep(3)
    status = m.at(prefix, '%s auth status --json' % f.PB, check=False).stdout
    results.append(step('cli-approval-done', code == '0' and '"signed_in": true' in status.replace('":true', '": true'),
                        'the waiting pb auth login exits 0 and the machine is signed in (exit %s)' % (code or 'still waiting')))
    return results


def console_account(m):
    """One throwaway account with one machine: every page reads cleanly (F2-07), the CLI approval page (F2-02), the Add
    machine dialog with a real pairing through its one-liner (F1-12, F1-01 Linux), keep awake from the machine page (F2-06),
    API tokens (F18-09), machine tags (F18-11), the machine's network times (F9-06) and rename/remove of a machine (F2-07)."""
    f = fx()
    _, state = m.account('acct')
    name, machine_id, prefix = machine(m, 'ac', state)
    results = []
    results += cli_approval(m, state, prefix)
    added, made = add_machine_live(m, state)
    results += added
    results.append(console_rows.script(m, 'all-pages', 'console_all_pages.mjs', str(SCRIPTS / 'check-all-pages.js'), machine_id, state=state, timeout=1200))
    results.append(console_rows.script(m, 'keep-awake', 'console_keep_awake.mjs', machine_id, state=state, timeout=600))
    results.append(paste(m, 'check-access-policy.js', '/settings/api-tokens', state))
    results.append(paste(m, 'check-machine-tags.js', '/machines/' + machine_id, state, {'__machine': machine_id}))
    edges = json.loads(m.at(prefix, '%s edge list --json' % f.PB, timeout=180).stdout)
    relays = json.loads(m.at(prefix, '%s relay list --json' % f.PB, timeout=180).stdout)
    cli = {'edges': (edges.get('data') or edges).get('regions') or [], 'relays': (relays.get('data') or relays).get('relays') or []}
    results.append(console_rows.script(m, 'network', 'console_network.mjs', machine_id, state=state, env={'CLI_JSON': json.dumps(cli)}, timeout=900))
    results.append(paste(m, 'check-machines-paging.js', '/', state))
    if made:
        added_name, added_id = made
        alias = data(m, state, '/v1/machines/' + added_id)['alias']
        results.append(paste(m, 'check-machine-lifecycle.js', '/machines/' + added_id, state, {'__machineId': added_id, '__alias': alias}, timeout=600))
        results.append(signed_out_on_device('removed-on-device', 'the machine removed in the console',
                                            lambda command: container(f, added_name, command, check=False).stdout, '.local/bin/pb'))
    else:
        results.append(step('machine-lifecycle', False, 'no second machine to rename and remove'))
    return console_rows.verdict('console-account', results)


SIGNED_OUT = ('signed out', 'no longer valid', 'authentication_required', 'not signed in')


def signed_out_on_device(label, what, read, pb, seconds=300):
    """The step after the console took a machine away: its own `pb status` says it is signed out. The client waits two
    minutes of refusals before it says so (refreshRevocationConfirmation). read(command) runs on the machine."""
    started = time.monotonic()
    while True:
        status = read('%s status 2>&1; true' % pb)
        out = any(phrase in status.lower() for phrase in SIGNED_OUT)
        if out or time.monotonic() - started > seconds:
            break
        time.sleep(10)
    waited = time.monotonic() - started
    account = ''.join(read('%s auth status --json 2>&1; true' % pb).split())
    said = next((line for line in status.splitlines() if any(p in line.lower() for p in SIGNED_OUT)), ' '.join(status.split())[:200])
    return step(label, out, '%s says so on the device after %d s: %s (pb auth status signed_in: %s)'
                % (what, waited, console_rows.redact(said.strip())[:200], 'false' if '"signed_in":false' in account else 'true' if '"signed_in":true' in account else '?'))


def signup(m, label):
    """(directory, state file) of a throwaway account whose deletion is the row's subject: no automatic cleanup yet."""
    import tempfile
    from pathlib import Path
    directory = str(Path(tempfile.gettempdir()) / ('fx-%s-%s' % (label, m.run_id)))
    m.node('fresh_account.mjs', 'signup', directory, state=directory + '/state.json')
    return directory, directory + '/state.json'


def remove_account_if_alive(m, directory):
    import shutil
    status, _ = api(m, directory + '/state.json', 'GET', '/v1/me')
    if status == 200:
        m._delete_account(directory)
    else:
        shutil.rmtree(directory, ignore_errors=True)


def public_url_status(url):
    done = subprocess.run(['curl', '-sS', '-m', '20', '-o', '/dev/null', '-w', '%{http_code}', url], capture_output=True, text=True, timeout=40)
    return done.stdout.strip() or 'unreachable'


def console_account_delete(m):
    """F2-09 on a throwaway account with a machine, a live preview, a tunnel, an ENV vault and an API token: a stale
    sign-in is refused; after signing in again the console deletes it (check-account-deletion.mjs: what goes, the typed
    email, 204, signed out); then the machine reads signed out, the preview and tunnel stop serving, the old token is
    refused, the confirmation mail arrives, and the address can sign up again as a new, empty account."""
    f = fx()
    started = time.monotonic()
    directory, state = signup(m, 'delete')
    m.callback(remove_account_if_alive, m, directory)  # runs after the machine's own cleanup, which needs the session
    with open(directory + '/account.json') as handle:
        email = json.load(handle)['email']
    name, machine_id, prefix = machine(m, 'del', state)
    serve(m, prefix, 18088)
    shown = lambda done: (lambda v: v.get('data') or v)(json.loads(done.stdout))
    preview = shown(m.at(prefix, '%s preview 18088 --background --ttl 1h --json' % f.PB, timeout=180))
    tunnel = shown(m.at(prefix, '%s tunnel create del-%s --port 18088 --wait --json' % (f.PB, m.run_id[-4:]), timeout=300))
    tunnel_url = tunnel.get('stable_endpoint') or (tunnel.get('tunnel') or {}).get('stable_endpoint') or ''
    vault = f.secret_file(m, prefix, 'vault.pw')
    m.at(prefix, '%s env init --password-stdin' % f.PB, stdin=vault)
    token = m.node('mint_api_token.mjs', 'fx-delete', state=state).stdout.strip()
    urls = [u for u in (preview.get('endpoint'), tunnel_url) if u]
    before = [public_url_status(u) for u in urls]
    results = [step('delete-fixture', len(urls) == 2 and before == ['200', '200'], 'the preview and the tunnel serve before the deletion (%s)' % before)]
    old = old_session(m, state, directory)
    time.sleep(max(0, 320 - (time.monotonic() - started)))  # the server asks for a sign-in from the last five minutes
    refused, body = api(m, state, 'DELETE', '/v1/account', {'confirm_email': email})
    results.append(step('delete-stale-sign-in', refused in (401, 403), 'a sign-in older than five minutes cannot delete the account (HTTP %s %s)'
                        % (refused, ((body or {}).get('error') or {}).get('code', '') if isinstance(body, dict) else '')))
    m.node('fresh_account.mjs', 'reauth', directory, state=state, timeout=900)
    results.append(console_rows.script(m, 'account-deletion', '../../console/scripts/check-account-deletion.mjs', state=state,
                                       env=dict(browser_env(), EMAIL=email), timeout=600))
    results.append(signed_out_on_device('delete-device', 'the deleted account\'s machine',
                                        lambda command: m.at(prefix, command, check=False, timeout=60).stdout, f.PB))
    after, deadline = [], time.monotonic() + 180
    while time.monotonic() < deadline:
        after = [public_url_status(u) for u in urls]
        if '200' not in after:
            break
        time.sleep(10)
    results.append(step('delete-published', len(urls) == 2 and '200' not in after, 'the preview and the tunnel stop serving (%s)' % after))
    anonymous = subprocess.run(['curl', '-sS', '-m', '30', '-o', '/dev/null', '-w', '%{http_code}', '-H', '@-', f.ENV['SMOKE_OPERATOR_API'] + '/v1/me'],
                               input='Authorization: Bearer %s' % token, capture_output=True, text=True, timeout=60).stdout.strip()
    results.append(step('delete-token', anonymous == '401', 'an API token made before the deletion is refused (HTTP %s)' % anonymous))
    results.append(step('delete-old-session', api(m, old, 'GET', '/v1/me')[0] == 401, 'the session saved before the deletion is refused'))
    results.append(console_rows.script(m, 'delete-mail-and-again', 'console_deleted_account.mjs', directory, state=state, timeout=900))
    return console_rows.verdict('console-account-delete', results)


def old_session(m, state, directory):
    import shutil
    copy = directory + '/before.json'
    shutil.copyfile(state, copy)
    os.chmod(copy, 0o600)
    return copy


def console_tunnels(m):
    """F8-08, F8-16 (console), W-10, W-11: a tunnel made on the machine is listed, renamed, paused, resumed and its
    hostname routes edited (check-tunnels.js); header rules (console_route_rules.mjs); IP rules on the tunnel and on a
    preview, and the flood notice (check-ip-rules.mjs)."""
    f = fx()
    _, state = m.account('tun')
    name, _, prefix = machine(m, 'tn', state)
    serve(m, prefix, 18088)
    m.at(prefix, '%s tunnel create test-f808 --port 18088 --wait --json' % f.PB, timeout=300)
    results = [paste(m, 'check-tunnels.js', '/tunnels', state, {'__tunnel': 'test-f808', '__port': '18088'}, timeout=900)]
    tunnel = next(t for t in data(m, state, '/v1/tunnels?workspace=personal')['items'] if t['name'] == 'test-f808')['id']
    results.append(json_steps(console_rows.script(m, 'route-rules', 'console_route_rules.mjs', tunnel, state=state, timeout=600)))
    made = json.loads(m.at(prefix, '%s preview 18088 --background --ttl 30m --json' % f.PB, timeout=180).stdout)
    preview = (made.get('data') or made)['id']
    results.append(node_check(m, 'check-ip-rules.mjs', state=state, env={'TUNNEL': tunnel, 'PREVIEW': preview}, timeout=900))
    m.at(prefix, '%s preview stop %s --json' % (f.PB, preview), check=False, timeout=120)
    return console_rows.verdict('console-tunnels', results)


def json_steps(result):
    """console_route_rules.mjs prints one JSON line with its steps."""
    label, code, output = result
    lines = [output]
    record = fx().last_json(output)
    for item in record.get('steps', []):
        lines.append('%s  %s %s' % (item['status'], item['step'], item.get('detail', '')[:150]))
    return label, code, '\n'.join(lines)


def console_files(m):
    """F6-06 (own machines): a file sent to a machine is listed on its Files screen, downloads with the exact bytes and
    is removed from there; an offline machine says so (final_console_files.sh, check-files-machine.js)."""
    f = fx()
    _, state = m.account('files')
    sender, _, sender_prefix = machine(m, 'fa', state)
    receiver, _, _ = machine(m, 'fb', state)
    gone, gone_id, _ = machine(m, 'fc', state)
    f.ready(sender_prefix, receiver)
    f.remote('%s stop -t 30 %s' % (f.DOCKER, gone), timeout=120)
    deadline = time.monotonic() + 420
    while data(m, state, '/v1/machines/' + gone_id).get('online') and time.monotonic() < deadline:
        time.sleep(10)
    result = shell(m, 'files', [str(HERE / 'final_console_files.sh'), f.HOST, sender, receiver], state, timeout=900)
    f.remote('%s start %s' % (f.DOCKER, gone), check=False, timeout=120)  # so it can revoke its own record
    time.sleep(20)
    return console_rows.verdict('console-files', [result])


def pending_requests(m, state, team, names, seconds=240):
    deadline, found = time.monotonic() + seconds, set()
    while time.monotonic() < deadline:
        status, body = api(m, state, 'GET', '/v1/team-inbox/requests?workspace=' + team)
        if status == 200:
            listed = (body.get('data') or body).get('requests') or []
            found = {x['basename'] for r in listed if r['status'] == 'pending' for x in r['files']}
            if set(names) <= found:
                return True
        time.sleep(5)
    return False


def team_inbox(m, state, team, member_id, own, member):
    """F6-06 (team requests): a teammate's two sends wait; the owner approves one and declines one on the Files page
    (check-team-inbox.mjs); only the approved file arrives."""
    f = fx()
    own_name, own_id, own_prefix = own
    member_name, _, member_prefix = member
    generation = lambda: f.team_generation(m, own_prefix, team)
    m.at(own_prefix, '%s team machine share %s %s --generation %s --json' % (f.PB, team, own_id, generation()))
    m.at(own_prefix, '%s team machine grant %s %s --member %s --capability files --generation %s --json' % (f.PB, team, own_id, member_id, generation()))
    m.callback(lambda: m.at(own_prefix, '%s team machine unshare %s %s --generation %s --json' % (f.PB, team, own_id, generation()), check=False))
    m.at(own_prefix, '%s inbox policy manual --json' % f.PB)
    names = ['approve-%s.txt' % m.run_id, 'decline-%s.txt' % m.run_id]
    time.sleep(20)  # the member's machine list learns the shared machine
    for filename in names:
        m.at(member_prefix, 'echo team-inbox >%s && (nohup %s send %s --to %s --json >%s.out 2>&1 &)' % (filename, f.PB, filename, own_name, filename))
    if not pending_requests(m, state, team, names):
        said = m.at(member_prefix, 'cat %s.out %s.out 2>&1; true' % tuple(names), check=False).stdout
        return [step('team-inbox-requests', False, 'the teammate\'s two sends never waited for approval: ' + ' '.join(said.split())[-300:])]
    results = [node_check(m, 'check-team-inbox.mjs', state=state, env={'TEAM': team, 'APPROVE_FILE': names[0], 'DECLINE_FILE': names[1]}, timeout=600)]
    inbox = json.loads(m.at(own_prefix, '%s inbox path --json' % f.PB).stdout)
    inbox = (inbox.get('data') or inbox)['path']
    listing, deadline = '', time.monotonic() + 180
    while time.monotonic() < deadline:
        listing = m.at(own_prefix, 'find %s -type f -name "*%s*"; true' % (shlex.quote(inbox), m.run_id), check=False).stdout
        if names[0] in listing:
            break
        time.sleep(5)
    results.append(step('team-inbox-delivery', names[0] in listing and names[1] not in listing, 'only the approved file arrives: ' + ' '.join(listing.split())[-200:]))
    return results


def console_team(m):
    """One paid throwaway team (owner, one member, Stripe sandbox): the Teams screens (F12-07), the default access line while
    enrolling (F12-04), team file requests approved and declined in the console (F6-06), port names and the front desk
    (F27-01, W-15), team ENV vaults in the browser (F10-08) and the refusal to delete an account that owns a team with
    other members (F2-09)."""
    f = fx()
    _, owner_state = m.account('towner')
    member_email, member_state = m.account('tmember')
    own = machine(m, 'to', owner_state)
    member = machine(m, 'tm', member_state)
    prefix = own[2]
    f.stage(prefix)
    f.ready(prefix, own[0])
    f.member_profile(m, prefix, member_state)
    team = f.paid_team(m, prefix, member_email)
    member_id = data(m, member_state, '/v1/me')['id']
    results = team_inbox(m, owner_state, team, member_id, own, member)
    results.append(paste(m, 'check-teams.js', '/team', owner_state, {'__team': team}, timeout=900))
    results.append(paste(m, 'check-team-enroll.js', '/', owner_state))
    results.append(node_check(m, 'check-port-names.mjs', state=owner_state, timeout=900))
    results.append(node_check(m, 'check-env-team-vault.mjs', state=owner_state, env={'STATE_A': owner_state, 'STATE_B': member_state}, timeout=1500))
    # a fresh sign-in first: the server refuses a stale one whatever the account owns
    m.node('fresh_account.mjs', 'reauth', os.path.dirname(owner_state), state=owner_state, timeout=900)
    refused, body = api(m, owner_state, 'DELETE', '/v1/account', {'confirm_email': data(m, owner_state, '/v1/me')['email']})
    code = ((body or {}).get('error') or {}).get('code', '') if isinstance(body, dict) else ''
    page = console_rows.script(m, 'delete-refused-page', 'console_text.mjs', '/account/delete', '[Tt]eam', state=owner_state, timeout=300)
    results.append(step('delete-owning-team', 400 <= refused < 500 and refused not in (401, 429) and 'auth' not in code,
                        'deleting the account that owns a team with a member is refused for the team (HTTP %s %s)' % (refused, code)))
    results.append(page)
    return console_rows.verdict('console-team', results)


def console_env(m):
    """F10-06, F10-07, F10-08 (one person): vault setup, recovery code and recovery (check-env-vault-recovery.mjs); named
    vaults, then the Environment screens' unlock/add/list/remove with the value never on the page (check-named-vaults.mjs,
    check-environment.js); the setup form and a refused key rotation (check-env-form-and-rotate-error.mjs)."""
    results = []
    _, first = m.account('env-recovery')
    results.append(node_check(m, 'check-env-vault-recovery.mjs', state=first, timeout=1200))
    _, second = m.account('env-named')
    password_file = os.path.join(console_rows.private_directory(m, 'console-env-'), 'vault.pw')
    results.append(node_check(m, 'check-named-vaults.mjs', state=second, env={'PASSWORD_FILE': password_file}, timeout=1200))
    if os.path.exists(password_file):
        with open(password_file) as handle:
            results.append(paste(m, 'check-environment.js', '/environment', second, secret_globals={'__vaultPassword': handle.read().strip()}, timeout=600))
    else:
        results.append(step('environment-screens', False, 'check-named-vaults.mjs left no vault password'))
    _, third = m.account('env-form')
    results.append(node_check(m, 'check-env-form-and-rotate-error.mjs', state=third, timeout=900))
    return console_rows.verdict('console-env', results)


def console_config_sync(m):
    """F11-05: the Config sync screens against a real assignment as it changes: in sync, waiting for approval, one
    conflict, resolved (console_config_fixture.py on the machine, check-config-sync.js in the browser each time).
    GitHub Disconnect then Connect needs a person's GitHub sign-in and is not covered."""
    f = fx()
    _, state = m.account('cfgsync')
    name, _, prefix = machine(m, 'cs', state)
    f.remote('%s exec %s sh -c %s' % (f.DOCKER, name, shlex.quote('apt-get update -qq >/dev/null && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq git >/dev/null')), timeout=600)
    f.stage(prefix)
    results = []
    for phase, checked in (('synced', True), ('review', True), ('approve', False), ('conflict', True), ('resolve', True)):
        code, out = f.on_machine(prefix, 'console_config_fixture.py', [name, phase], 600)
        results.append(step('config-' + phase, code == 0, 'the machine reaches "%s"%s' % (phase, '' if not code else ': ' + str(f.last_json(out).get('detail', ' '.join(out.split())[-250:])))))
        if code:
            break
        if checked:
            time.sleep(20)  # the machine's report reaches the server
            results.append(paste(m, 'check-config-sync.js', '/config', state, label='config-screens-' + phase, timeout=600))
    return console_rows.verdict('console-config-sync', results)


def terminal_check(m, row, machine_id, state, label, env=None, timeout=900):
    return node_check(m, 'check-terminal-rows.mjs', state=state, label=label, env=dict({'ROW': row, 'MACHINE': machine_id}, **(env or {})), timeout=timeout)


def console_terminal(m):
    """F3-07 on a Linux machine (open, type, resize, reconnect, close) and F3-08 (the People panel: real names, join code,
    a guest without an account, a teammate with one, presence): check-terminal-rows.mjs f307 and f308."""
    _, state = m.account('term')
    member_email, member_state = m.account('term-member')
    _, machine_id, _ = machine(m, 'te', state)
    results = [terminal_check(m, 'f307', machine_id, state, 'f307-linux'),
               terminal_check(m, 'f308', machine_id, state, 'f308', env={'MEMBER_STATE': member_state, 'MEMBER_EMAIL': member_email}, timeout=1200)]
    return console_rows.verdict('console-terminal', results)


def console_terminal_fleet(m):
    """F3-07 against the run's own macOS and Windows machines: SMOKE_CONSOLE_TERMINAL_TARGETS="name,name:powershell" (names
    of paired machines of the smoke team's owner). The browser is the console browser (console_browser.sh)."""
    state = fx().path('SMOKE_OPERATOR_STATE')
    wanted = [t for t in (os.environ.get('SMOKE_CONSOLE_TERMINAL_TARGETS') or fx().ENV.get('SMOKE_CONSOLE_TERMINAL_TARGETS', '')).split(',') if t]
    if not wanted:
        return False, 'SMOKE_CONSOLE_TERMINAL_TARGETS names no macOS or Windows machine'
    results = []
    for target in wanted:
        name, _, kind = target.partition(':')
        found = [i for i in data(m, state, '/v1/machines?workspace=personal&limit=100&q=' + name)['items'] if i['alias'] == name]
        if len(found) != 1 or not found[0].get('online'):
            results.append(step('f307-' + name, False, '%s is not one online machine of the account' % name))
            continue
        results.append(terminal_check(m, 'f307', found[0]['id'], state, 'f307-' + name, env={'MACHINE_SHELL': kind} if kind else None))
    return console_rows.verdict('console-terminal-fleet', results)


def console_site(m):
    """Pages without an account (legal pages, /auth/failed, /auth/deleted), then on a throwaway account Network > Your
    regions (create, rename, remove) and GitHub connect up to GitHub's own sign-in with the refusals of /github/connect and
    /github/callback."""
    f = fx()
    results = [console_rows.script(m, 'site', 'console_site.mjs', state='', env={'MARKETING_ORIGIN': f.ENV.get('SMOKE_MARKETING_ORIGIN', 'https://paperboat.cloud')}, timeout=600)]
    _, state = m.account('site')
    results.append(console_rows.script(m, 'regions', 'console_regions.mjs', state=state, timeout=600))
    results.append(console_rows.script(m, 'github-connect', 'console_github.mjs', state=state, timeout=600))
    return console_rows.verdict('console-site', results)


def console_access_login(m):
    """The /access-login page: a private preview in the browser (console_access_login.mjs): the owner comes back to the
    page, a signed-out browser is asked to sign in, another account gets 404."""
    f = fx()
    _, state = m.account('access')
    _, stranger = m.account('access-other')
    _, _, prefix = machine(m, 'al', state)
    serve(m, prefix, 18088)
    made = json.loads(m.at(prefix, '%s preview 18088 --private --background --ttl 20m --json' % f.PB, timeout=180).stdout)
    made = made.get('data') or made
    try:
        result = console_rows.script(m, 'access-login', 'console_access_login.mjs', made['endpoint'], state=state,
                                     env={'STRANGER_STATE': stranger, 'MARKER': 'console-row'}, timeout=600)
    finally:
        m.at(prefix, '%s preview stop %s --json' % (f.PB, made['id']), check=False, timeout=120)
    return console_rows.verdict('console-access-login', [result])


def console_announcements(m):
    """A banner and a notice made with the operator API for one throwaway account only, seen, read and dismissed in its
    console (console_announcements.mjs), not seen by another account, then retracted and gone."""
    import datetime
    import region_drain
    f = fx()
    _, state = m.account('ann')
    _, other = m.account('ann-other')
    user = data(m, state, '/v1/me')['id']
    operator = region_drain.Operator(f.ENV['SMOKE_OPERATOR_API'], os.path.expanduser(f.ENV.get('SMOKE_OPERATOR_CRED', '~/.config/paperboat-dev/operator-cred')))
    operator.login()
    titles = {'banner': 'Row banner %s' % m.run_id, 'notice': 'Row notice %s' % m.run_id}
    ends = (datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(hours=1)).strftime('%Y-%m-%dT%H:%M:%SZ')
    made, results = [], []

    def retract():
        for announcement in made[:]:
            status, _ = operator.call('POST', '/v1/admin/announcements/%s/retract' % announcement, {}, target=announcement)
            if status == 200:
                made.remove(announcement)
        if made:
            m.leftovers.append('announcements NOT retracted (they end by %s): %s' % (ends, ','.join(made)))

    m.callback(lambda: made and retract())
    for kind, title in titles.items():
        status, body = operator.call('POST', '/v1/admin/announcements', {'kind': kind, 'title': title, 'body': 'Made by a harness row for one test account.',
                                     'link_url': '/network', 'severity': 'info', 'dismissible': True, 'audience_kind': 'user', 'audience_value': user,
                                     'ends_at': ends}, target='announcements')
        results.append(step('announce-' + kind, status == 200, 'the operator API makes a %s for one account (HTTP %s)' % (kind, status)))
        if status == 200:
            made.append(body['data']['id'])
    env = {'OTHER_STATE': other, 'BANNER': titles['banner'], 'NOTICE': titles['notice']}
    if len(made) == 2:
        results.append(console_rows.script(m, 'announcements', 'console_announcements.mjs', 'shown', state=state, env=env, timeout=600))
    retract()
    results.append(console_rows.script(m, 'retracted', 'console_announcements.mjs', 'gone', state=state, env=env, timeout=300))
    return console_rows.verdict('console-announcements', results)


def admin_panel(m):
    """The operator admin panel, read only (console_admin.mjs): sign-in with the second factor and every page on the live
    deployment. Operator only; its write actions are checked by admin/scripts/check-admin.mjs against a fixture."""
    f = fx()
    return console_rows.verdict('admin-panel', [console_rows.script(m, 'admin', 'console_admin.mjs', state='', env={
        'ADMIN_ORIGIN': f.ENV.get('SMOKE_ADMIN_ORIGIN', 'https://admin.pprbt.dev'),
        'OPERATOR_CRED': os.path.expanduser(f.ENV.get('SMOKE_OPERATOR_CRED', '~/.config/paperboat-dev/operator-cred'))}, timeout=900)])


CHECKS = {'console-public': console_public, 'console-account': console_account, 'console-account-delete': console_account_delete,
          'console-previews': console_previews, 'console-tunnels': console_tunnels, 'console-files': console_files, 'console-team': console_team,
          'console-env': console_env, 'console-config-sync': console_config_sync, 'console-terminal': console_terminal,
          'console-terminal-fleet': console_terminal_fleet, 'console-site': console_site,
          'console-access-login': console_access_login, 'console-announcements': console_announcements, 'admin-panel': admin_panel}
