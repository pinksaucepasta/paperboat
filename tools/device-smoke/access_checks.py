#!/usr/bin/env python3
"""The access policy, API token and tunnel/preview rule rows of fixture_checks.py. Each makes its own throwaway account(s) and
machine containers on SMOKE_FIXTURE_HOST, runs the existing check script against them and removes everything on every exit path.

  policy-cli           F18-01, F18-10, C244-C253, C289, C290: policy_cli.sh, then what the file format row promises: the served schema is
                       the printed one; too large, too many rules, unknown field and failing test files are refused and never saved;
                       F18-05 a save made against an old version is refused; F18-06 the token's expiry is listed and a revoked token is refused.
  policy-token         F18-06: policy_token.py (scoped token signs a second machine in; scope, revoke, tag removal).
  machine-tags         F18-11, C262: machine_tags.py.
  policy-team          F18-02, F18-04, F18-05, F18-07, F18-08: policy_team.py with two throwaway accounts and a team paid in Stripe's
                       sandbox, then group and tag rules, per-action deny over allow, private access by port, and a removed rule ending a running exec.
  tunnel-ip-rules      F8-13, C280: ip_rules_rows.py --kind tunnel; address A is this controller, B is SMOKE_OBSERVER_SSH.
  preview-ip-rules     F7-09, C281: ip_rules_rows.py --kind preview.
  tunnel-route-rules   F8-16: route_rules_rows.py.
  tunnel-origin        F8-09, F8-10, F8-11, F8-12: tunnel_origin_rows.py on the machine (headers, keep-alive, quiet WebSocket, TLS modes
                       with a pinned origin, gRPC, failure pages).
  tunnel-origin-proxies  F8-09: origin_tunnel_matrix.py, Caddy, Traefik, nginx and Apache forcing HTTPS behind real tunnels.
  tunnel-create        C279: create receipts (public and private), refusals in plain words, --help.

SMOKE_OBSERVER_SSH: a second host with curl and python3 whose public address differs from this controller's and from the machine's edge.
"""
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import time
import urllib.error
import urllib.request

import ct_env
import fixture_checks as fx

PB = fx.PB


def docker_exec(host, name, command, user='root', timeout=600, check=True):
    """A command in the fixture container as `user` (setup the test user cannot do)."""
    return fx.run(['ssh', '-o', 'BatchMode=yes', host, '%s exec -u %s %s sh -c %s' % (ct_env.CT_DOCKER, user, name, shlex.quote(command))],
                  timeout=timeout, check=check)


def solo(m, labels, names=None):
    """One throwaway account with a paired container per label: (email, state, [(name, machine id, prefix)])."""
    email, state = m.account(labels[0])
    made = [m.machine(label, state=state, name=names and '%s-%s' % (names[i], m.run_id)) for i, label in enumerate(labels)]
    for name, _, prefix in made:
        fx.stage(prefix)
        fx.ready(prefix, name)
    return email, state, made


def script(argv, timeout, env=None):
    """Runs a harness script on this controller; (exit code, stdout, both streams)."""
    try:
        done = subprocess.run([sys.executable, str(fx.HERE / argv[0]), *argv[1:]], capture_output=True, text=True, timeout=timeout,
                              stdin=subprocess.DEVNULL, env=env)
    except subprocess.TimeoutExpired as error:
        out = (error.stdout or b'')
        out = out.decode(errors='replace') if isinstance(out, bytes) else out
        return 124, out, out + '\nFAIL timed out after %ds' % timeout
    return done.returncode, done.stdout, done.stdout + done.stderr


def redact(text):
    return re.sub(r'pbat_[A-Za-z0-9_-]+', 'pbat_<redacted>', text)


def line_verdict(check, code, text, summary, minimum):
    """The verdict of a script that prints PASS/FAIL lines: it must exit 0, print at least `minimum` of them and no FAIL."""
    text = redact(text)
    lines = [l for l in text.splitlines() if l.startswith(('PASS ', 'FAIL '))]
    failed = [l for l in lines if l.startswith('FAIL ')]
    log = fx.keep(check, text)
    if code == 0 and not failed and len(lines) >= minimum:
        return True, '%s; %d/%d steps, log %s' % (summary, len(lines), len(lines), log)
    why = '; '.join(l[5:150] for l in failed[:4]) or ' '.join(text.split())[-250:]
    return False, '%d of %d steps failed (exit %d): %s log %s' % (len(failed), len(lines), code, why, log)


def report_verdict(check, code, stdout, text, summary):
    """The verdict of a script that prints one JSON report {"status", "steps": [{"step", "status", "detail"}]}."""
    log = fx.keep(check, redact(text))
    try:
        report = json.loads(stdout[stdout.index('{'):])
    except ValueError:
        return False, 'no report (exit %d): %s log %s' % (code, ' '.join(text.split())[-280:], log)
    steps = report.get('steps', [])
    failed = ['%s (%s)' % (s['step'][:70], str(s.get('detail', ''))[:150]) for s in steps if s['status'] != 'PASS']
    if code == 0 and report.get('status') == 'PASS' and steps and not failed:
        return True, '%s; %d/%d steps, log %s' % (summary, len(steps), len(steps), log)
    return False, '%d/%d steps failed: %s log %s' % (len(failed), len(steps), '; '.join(failed)[:270] or 'status %s' % report.get('status'), log)


# ---- policy file and API tokens ----

# runs in the container: one API request with the token in ~/api.tok (never printed)
API_PROBE = r'''
import json, sys, urllib.error, urllib.request
base, method, path = sys.argv[1:4]
body = sys.stdin.read() or None
request = urllib.request.Request(base.rstrip('/') + path, data=body.encode() if body else None, method=method, headers={
    'Authorization': 'Bearer ' + open('api.tok').read().strip(), 'Content-Type': 'application/json', 'User-Agent': 'smoke-check',
    'Idempotency-Key': 'smoke-%s' % __import__('os').urandom(8).hex()})
try:
    with urllib.request.urlopen(request, timeout=60) as response:
        print(json.dumps({'status': response.status, 'body': json.loads(response.read() or b'{}')}))
except urllib.error.HTTPError as error:
    print(json.dumps({'status': error.code, 'body': json.loads(error.read() or b'{}')}))
'''


def server_url(m, prefix):
    shown = json.loads(m.at(prefix, '%s config show --json' % PB).stdout)
    return (shown.get('data') or shown)['server_url']


def data(text):
    value = json.loads(text)
    return value.get('data', value) if isinstance(value, dict) else value


def policy_format(m, prefix, email, target, steps):
    def step(name, ok, detail=''):
        steps.append('%s %s%s' % ('PASS' if ok else 'FAIL', name, '' if ok else ': ' + ' '.join(redact(str(detail)).split())[-300:]))

    def pb(args, stdin=None):
        done = m.at(prefix, '%s %s' % (PB, args), check=False, timeout=150, stdin=stdin)
        return done.returncode, done.stdout, done.stdout + done.stderr

    def version():
        return data(pb('policy get --workspace personal --json')[1])['version']

    base = server_url(m, prefix)
    with urllib.request.urlopen(urllib.request.Request(base.rstrip('/') + '/v1/policy/schema', headers={'User-Agent': 'smoke-check'}), timeout=30) as response:
        served, kind = json.loads(response.read()), response.headers.get('Content-Type', '')
    printed = json.loads(pb('policy schema')[1])
    step('F18-01 the server serves the schema the CLI prints', served == printed and 'schema+json' in kind, '%s %s' % (kind, str(served)[:120]))
    before = version()
    rule = {'src': ['user:' + email], 'dst': ['machine:' + target], 'allow': ['exec']}
    refused = (
        ('a file over 256 KiB', '// ' + 'x' * (300 * 1024) + '\n{}\n', ('larger than the limit',)),
        ('more than 1000 rules', json.dumps({'rules': [rule] * 1001}), ('/rules', '1000')),
        ('an unknown field', json.dumps({'rulez': []}), ('unknown_field', 'rulez')),
        ('an unknown action', json.dumps({'rules': [dict(rule, allow=['fly'])]}), ('/rules/0/allow/0', 'unknown_action')),
        ('a failing in-file test', json.dumps({'tests': [{'src': 'user:' + email, 'dst': 'machine:' + target, 'deny': ['exec']}]}), ('1 failed',)),
    )
    for label, text, words in refused:
        for command in ('policy test - --workspace personal', 'policy set - --workspace personal --yes --message smoke-refused'):
            if command.startswith('policy test'):
                m.at(prefix, 'cat > refused.jsonc', stdin=text)
                command = 'policy test refused.jsonc --workspace personal'
            code, _, out = pb(command, stdin=None if command.startswith('policy test') else text)
            step('F18-01 %s refuses %s' % (command.split()[1], label), code != 0 and all(w in out for w in words) and not re.search(r'panic|goroutine', out), 'exit %d %s' % (code, out[-300:]))
    step('F18-01 no refused file was saved (version unchanged)', version() == before, '%s -> %s' % (before, version()))
    # F18-05: two editors. The second save names the version it read and must be refused once the first has saved.
    code, out, both = pb('api-token create smoke-api --workspace personal --expires 1d --json')
    if code:
        step('create an unscoped API token', False, both)
        return
    token = data(out)
    secret = token.pop('secret')
    m.at(prefix, 'umask 077; cat > api.tok', stdin=secret)
    m.at(prefix, 'cat > api_probe.py', stdin=API_PROBE)

    def api(method, path, body=None):
        done = m.at(prefix, 'python3 api_probe.py %s %s %s' % (shlex.quote(base), method, shlex.quote(path)), stdin=json.dumps(body) if body else '', timeout=90)
        return json.loads(done.stdout.strip().splitlines()[-1])

    try:
        first = api('PUT', '/v1/workspaces/personal/policy', {'policy': '{}\n// first editor\n', 'expected_version': before, 'message': 'first editor'})
        step('F18-05 the first editor saves', first['status'] == 200, first)
        stale = api('PUT', '/v1/workspaces/personal/policy', {'policy': '{}\n// second editor\n', 'expected_version': before, 'message': 'second editor'})
        step('F18-05 a stale save is refused (409 policy_version_conflict)', stale['status'] == 409 and 'policy_version_conflict' in json.dumps(stale), stale)
        kept = pb('policy get --workspace personal')[1]
        step('F18-05 the stale save changed nothing', 'first editor' in kept and 'second editor' not in kept, kept[-200:])
        pb('policy restore %s --workspace personal --yes' % before)
        listed = data(pb('api-token list --workspace personal --json')[1])
        items = listed.get('api_tokens') or listed.get('items') or [] if isinstance(listed, dict) else listed
        entry = next((t for t in items if t.get('name') == 'smoke-api'), {})
        left = 0
        if entry.get('expires_at'):
            import datetime
            left = (datetime.datetime.fromisoformat(entry['expires_at'].replace('Z', '+00:00')) - datetime.datetime.now(datetime.timezone.utc)).total_seconds()
        step('F18-06 the list shows the expiry asked for (1 day) and no secret', 85_000 < left <= 86_700 and secret not in json.dumps(listed), entry)  # 5 min of clock difference
        for bad in ('0d', '4000d'):
            code, _, out = pb('api-token create smoke-bad --workspace personal --expires %s' % bad)
            step('F18-06 expiry %s is refused' % bad, code != 0 and 'pbat_' not in out and re.search(r'expir|duration', out, re.I) is not None, redact(out))
        code, _, out = pb('api-token revoke smoke-api --workspace personal --yes')
        step('F18-06 revoke the token', code == 0, out)
        dead = api('GET', '/v1/workspaces/personal/policy')
        step('F18-06 a revoked token is refused (401)', dead['status'] == 401, dead['status'])
    finally:
        m.at(prefix, 'rm -f api.tok api_probe.py refused.jsonc', check=False)
    for ident, group, wanted in (('C290', 'policy', ('get', 'set', 'test', 'explain', 'versions', 'restore', 'schema')), ('C289', 'api-token', ('create', 'list', 'revoke'))):
        code, _, out = pb('%s --help' % group)
        step('%s pb %s --help lists %s' % (ident, group, ', '.join(wanted)), code == 0 and all(re.search(r'^\s+%s\s' % w, out, re.M) for w in wanted), out)
        code, _, out = pb('%s bogus' % group)
        step('%s pb %s refuses an unknown action in plain words' % (ident, group), code != 0 and not re.search(r'panic|goroutine', out), out)
        for sub in wanted:
            code, _, out = pb('%s %s --help' % (group, sub))
            if code or 'Usage' not in out:
                step('%s pb %s %s --help' % (ident, group, sub), False, out)


def policy_cli(m):
    email, _, [(name, _, prefix)] = solo(m, ['polcli'])
    packed = subprocess.run(['tar', '-C', str(fx.HERE), '-cf', '-', 'policy_cli.sh', 'ct_env.sh'], capture_output=True, check=True).stdout
    folder = '/tmp/policy-cli-%s' % m.run_id
    subprocess.run(['ssh', '-o', 'BatchMode=yes', fx.HOST, 'mkdir -p %s && tar -xf - -C %s' % (folder, folder)], input=packed, capture_output=True, check=True, timeout=120)
    m.callback(fx.remote, 'rm -rf %s' % folder, check=False)
    done = fx.remote('cd %s && CT=%s CT_DOCKER=%s SMOKE_CT_DOCKER=%s EMAIL=%s TARGET=%s bash policy_cli.sh' % (
        folder, name, shlex.quote(ct_env.CT_DOCKER), shlex.quote(ct_env.CT_DOCKER), shlex.quote(email), name), check=False, timeout=900)
    steps = []
    try:
        policy_format(m, prefix, email, name, steps)
    except (RuntimeError, ValueError, KeyError, OSError, subprocess.TimeoutExpired) as error:
        steps.append('FAIL the file format steps stopped: %s: %s' % (type(error).__name__, str(error)[:300]))
    text = done.stdout + done.stderr + '\n' + '\n'.join(steps)
    return line_verdict('policy-cli', done.returncode, text, 'pb policy and pb api-token against the live server, refused files never saved, stale save and revoked token refused', 25)


def policy_token(m):
    _, _, [(allowed, _, owner), (other, _, holder)] = solo(m, ['tokown', 'tokhold'])
    code, _, text = script(['policy_token.py', '--allowed', allowed, '--other', other, '--owner-cmd', owner, '--token-cmd', holder, '--pb', PB], 1500)
    return line_verdict('policy-token', code, text, 'a scoped token works only inside its scope, ends when revoked and loses a tag it no longer has', 10)


def machine_tags(m):
    _, _, [(name, _, prefix)] = solo(m, ['tags'])
    code, _, text = script(['machine_tags.py', '--owner-cmd', prefix, '--target', name, '--pb', PB], 900)
    return line_verdict('machine-tags', code, text, 'tags added, removed, set, listed by --tag, stored in the policy and shown by pb status', 9)


def team_env(m, prefix, team):
    """F18-07 needs the team's encrypted ENV scope: the owner's vault, then the scope."""
    secret = fx.secret_file(m, prefix, 'owner.pw')
    m.at(prefix, '%s env init --password-stdin' % PB, stdin=secret, timeout=150)
    done = m.at(prefix, '%s env team create %s --password-stdin < owner.pw' % (PB, team), check=False, timeout=150)
    if done.returncode:
        done = m.at(prefix, '%s env team create %s < owner.pw' % (PB, team), check=False, timeout=150)
    if done.returncode:
        raise RuntimeError('team ENV scope: ' + (done.stdout + done.stderr)[-250:])


def policy_team(m):
    _, owner_state = m.account('polown')
    member_email, member_state = m.account('polmem')
    name, machine_id, prefix = m.machine('powner', state=owner_state)
    member_name, _, member_prefix = m.machine('pmember', state=member_state)
    for machine, command in ((name, prefix), (member_name, member_prefix)):
        fx.stage(command)
        fx.ready(command, machine)
    team = fx.paid_team(m, prefix, None)
    invitation = json.loads(m.at(prefix, '%s team invite %s %s --generation %s --json' % (PB, team, shlex.quote(member_email), fx.team_generation(m, prefix, team))).stdout.strip().splitlines()[-1])
    invitation = invitation.get('data', invitation)
    m.at(member_prefix, '%s team accept %s --json' % (PB, invitation['invitation_id']), timeout=150)
    m.callback(lambda: m.at(member_prefix, '%s team leave %s --generation %s --json' % (PB, team, fx.team_generation(m, prefix, team)), check=False, timeout=150))
    setup = []
    try:
        team_env(m, prefix, team)
    except (RuntimeError, subprocess.TimeoutExpired) as error:
        setup.append('FAIL team ENV setup: %s' % str(error)[:300])
    code, _, text = script(['policy_team.py', '--team', team, '--machine-id', machine_id, '--machine-name', name,
                            '--owner-cmd', prefix, '--member-cmd', member_prefix, '--owner-pb', PB, '--member-pb', PB], 2400)
    steps = []
    try:
        policy_enforce(m, team, name, machine_id, prefix, member_prefix, member_email, steps)
    except (RuntimeError, ValueError, KeyError, subprocess.TimeoutExpired) as error:
        steps.append('FAIL the group and tag steps stopped: %s: %s' % (type(error).__name__, str(error)[:300]))
    return line_verdict('policy-team', code, '\n'.join(setup + [text] + steps),
                        'team enforcement, explain, review, audit, member rights, ENV rules, groups, tags, ports and session end', 30)


def port_refused(port, output):
    """What `pb access machine` says for a port the policy leaves out (the port is never offered), or a listener that carries nothing."""
    return re.search(r'offer port %d\b|denied|permission|not allowed|is listening on' % port, output, re.I) is not None


def policy_enforce(m, team, name, machine_id, owner, member, member_email, steps):
    """F18-02 beyond exec: a group may reach a tag; deny wins per action; private access follows the ports of the rule;
    removing the rule ends a command that is already running."""
    def step(label, ok, detail=''):
        steps.append('%s %s%s' % ('PASS' if ok else 'FAIL', label, '' if ok else ': ' + ' '.join(str(detail).split())[-300:]))

    def pb(prefix, args, timeout=150, stdin=None):
        done = m.at(prefix, '%s %s' % (PB, args), check=False, timeout=timeout, stdin=stdin)
        return done.returncode, done.stdout + done.stderr

    def generation():
        return fx.team_generation(m, owner, team)

    def save(document, message):
        code, out = pb(owner, 'policy set - --workspace %s --yes --message %s' % (team, message), stdin=json.dumps(document, indent=2) + '\n')
        if code:
            raise RuntimeError('policy set %s: %s' % (message, out[-300:]))

    def run(marker='f18-enforce'):
        return pb(member, 'exec --workspace %s %s -- echo %s' % (team, name, marker), timeout=90)

    def until(want, call, seconds=90):
        deadline, last = time.monotonic() + seconds, (1, '')
        while time.monotonic() < deadline:
            last = call()
            if (last[0] == 0) is want:
                return True, last[1]
            time.sleep(5)
        return False, last[1]

    start = json.loads(pb(owner, 'policy get --workspace %s --json' % team)[1].strip())
    start = start.get('data', start)['version']
    code, out = pb(owner, 'team machine share %s %s --generation %s --json' % (team, machine_id, generation()))
    if code:
        raise RuntimeError('share: ' + out[-300:])
    m.callback(lambda: m.at(owner, '%s team machine unshare %s %s --generation %s --json' % (PB, team, machine_id, generation()), check=False, timeout=150))
    document = {'groups': {'group:ops': ['user:' + member_email]}, 'tags': {'tag:smoke-ops': ['machine:' + name]},
                'rules': [{'src': ['group:ops'], 'dst': ['tag:smoke-ops'], 'allow': ['exec']},  # ports go only on a rule for private_access alone
                          {'src': ['group:ops'], 'dst': ['tag:smoke-ops'], 'allow': ['private_access'], 'ports': ['8765']}]}
    try:
        save(document, 'smoke-groups')
        ok, out = until(True, run)
        step('F18-02 a group rule on a tag lets the member exec', ok, out)
        for port in (8765, 8766):
            m.at(owner, 'nohup python3 -m http.server %d --bind 127.0.0.1 >/dev/null 2>&1 & sleep 1' % port)
        m.callback(lambda: m.at(owner, "pkill -f 'http.server 876' || true", check=False))
        for port in (8765, 8766):  # both answer on their own machine: a later refusal is Paperboat's, not a dead server
            done = m.at(owner, 'curl -fsS -m 5 --retry 10 --retry-connrefused --retry-delay 1 -o /dev/null http://127.0.0.1:%d/' % port, check=False)
            step('the test server on port %d answers on its own machine' % port, done.returncode == 0, done.stderr)

        def forwarded(port):
            local = 19000 + port % 100
            done = m.at(member, '(%s access machine %s --workspace %s --port %d --listen 127.0.0.1:%d > access-%d.log 2>&1 & echo $! > access-%d.pid); '
                        'rc=1; for i in $(seq 40); do curl -fsS -m 5 -o /dev/null http://127.0.0.1:%d/ && { rc=0; break; }; kill -0 $(cat access-%d.pid) 2>/dev/null || break; sleep 2; done; '
                        'kill $(cat access-%d.pid) 2>/dev/null; cat access-%d.log; exit $rc' % (PB, name, team, port, local, port, port, local, port, port, port), check=False, timeout=240)
            # stdout is pb's own log; curl's errors only matter when pb wrote nothing
            return done.returncode, done.stdout if done.stdout.strip() else done.stderr

        def said(log):
            return ' '.join(log.split())[:290]  # pb states what failed at the start
        code, out = forwarded(8765)
        step('F18-02 private access to the port the rule names works', code == 0, said(out))
        code, out = forwarded(8766)
        step('F18-02 private access to another port is refused', code != 0 and port_refused(8766, out), said(out))
        denied = json.loads(json.dumps(document))
        denied['rules'].append({'src': ['user:' + member_email], 'dst': ['machine:' + name], 'deny': ['exec']})
        save(denied, 'smoke-deny')
        ok, out = until(False, run)
        step('F18-02 a deny on the machine beats the group allow on its tag', ok, out)
        code, out = forwarded(8765)
        step('F18-02 the deny of one action leaves private access allowed', code == 0, said(out))
        save(document, 'smoke-groups-again')
        ok, out = until(True, run)
        step('F18-02 exec works again once the deny is removed', ok, out)
        outcome = {}

        def held():
            began = time.monotonic()
            code, out = pb(member, 'exec --workspace %s %s -- sh -c %s' % (team, name, shlex.quote('echo held-start; sleep 240; echo held-end')), timeout=300)
            outcome.update(code=code, out=out, seconds=time.monotonic() - began)

        import threading
        thread = threading.Thread(target=held, daemon=True)
        thread.start()
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline and m.at(owner, "pgrep -f 'sleep 240' >/dev/null", check=False).returncode:
            time.sleep(3)
        running = not m.at(owner, "pgrep -f 'sleep 240' >/dev/null", check=False).returncode
        step('F18-02 a long exec of the member is running', running, outcome)
        removed = time.monotonic()
        save({'groups': document['groups'], 'tags': document['tags'], 'rules': []}, 'smoke-remove')
        thread.join(150)
        within = not thread.is_alive()
        thread.join(150)  # past the command's own end, to tell "ended late" from "never ended"
        ran_out = re.search(r'^held-end\s*$', outcome.get('out', ''), re.M) is not None
        step('F18-02 removing the rule ends the running exec', running and within and outcome.get('code') != 0 and not ran_out,
             'ended within 150 s of the removal: %s; the 240 s command ran to its end: %s; pb exec exit %s after %.0f s in all' % (
                 within, ran_out, outcome.get('code'), outcome.get('seconds', -1)))
        gone = True
        for _ in range(12):
            gone = m.at(owner, "pgrep -f 'sleep 240' >/dev/null", check=False).returncode != 0
            if gone:
                break
            time.sleep(5)
        step('F18-02 the command it started on the machine is gone', gone, 'sleep 240 still runs')
        ok, out = until(False, run)
        step('F18-02 a new exec is refused after the removal', ok, out)
    finally:
        code, out = pb(owner, 'policy restore %s --workspace %s --yes --message smoke-enforce-cleanup' % (start, team))
        step('restore the policy', code == 0, out)


# ---- tunnel and preview rules ----

def observer():
    host = fx.ENV.get('SMOKE_OBSERVER_SSH', '')
    if not host:
        raise RuntimeError('set SMOKE_OBSERVER_SSH to a second host with curl and python3')
    # one shared connection: the observer is asked many times in a row, and a new ssh handshake through a jump host takes seconds
    return 'ssh -o BatchMode=yes -o ConnectTimeout=20 -o ControlMaster=auto -o ControlPersist=300 -o ControlPath=%s/pbobs-%%C %s' % (
        fx.tempfile.gettempdir(), shlex.quote(host))


def ip_rules(m, kind):
    second = observer()
    _, _, [(_, _, prefix)] = solo(m, ['ip' + kind[:3]])
    code, out, text = script(['ip_rules_rows.py', '--kind', kind, '--machine-cmd', prefix, '--observer-b-cmd', second, '--pb', PB], 1800)
    return report_verdict('%s-ip-rules' % kind, code, out, text, 'owner allow and deny lists hold at every edge from two real addresses')


def tunnel_ip_rules(m):
    return ip_rules(m, 'tunnel')


def preview_ip_rules(m):
    return ip_rules(m, 'preview')


def tunnel_route_rules(m):
    _, _, [(_, _, prefix)] = solo(m, ['route'])
    code, out, text = script(['route_rules_rows.py', '--machine-cmd', prefix, '--pb', PB], 1500)
    return report_verdict('tunnel-route-rules', code, out, text, 'header, rewrite and redirect rules reach the edge; edge headers cannot be spoofed')


def tunnel_create(m):
    _, _, [(_, _, prefix)] = solo(m, ['tcreate'])
    steps = []

    def step(label, ok, detail=''):
        steps.append('%s %s%s' % ('PASS' if ok else 'FAIL', label, '' if ok else ': ' + ' '.join(str(detail).split())[-300:]))

    def pb(args):
        done = m.at(prefix, '%s %s' % (PB, args), check=False, timeout=240)
        return done.returncode, done.stdout, done.stdout + done.stderr

    def delete(tunnel):
        code, out, _ = pb('tunnel delete %s --json' % tunnel)
        value = json.loads(out) if out.strip().startswith('{') else {}
        confirmation = (value.get('data') or value).get('confirmation_token')
        if confirmation:
            code = pb('tunnel delete %s --confirm %s --json' % (tunnel, confirmation))[0]
        step('delete %s' % tunnel, code == 0, out)

    name = 'tor-c279-%s' % m.run_id
    m.at(prefix, 'nohup python3 -m http.server 8765 --bind 127.0.0.1 >/dev/null 2>&1 & sleep 1')
    for label, arguments, private in (('public from --port', '%s --port 8765' % name, False),
                                      ('private from --from', '%s-p --from http://127.0.0.1:8765 --private' % name, True)):
        code, out, both = pb('tunnel create %s --duration 30m --wait --timeout 90s --json' % arguments)
        made = data(out).get('tunnel', {}) if code == 0 else {}
        endpoint = made.get('stable_endpoint', '')
        step('C279 create %s: the receipt names the tunnel, its id and https endpoint' % label,
             code == 0 and made.get('name') and made.get('id') and endpoint.startswith('https://'), both)
        if not made.get('id'):
            continue
        status = m.at(prefix, 'sleep 3; curl -sS -m 25 -o /dev/null -w "%%{http_code}" %s' % shlex.quote(endpoint), check=False, timeout=60).stdout.strip()
        if private:
            step('C279 a private tunnel refuses a visitor without access', status in ('401', '403', '404') or status.startswith('30'), status)
        else:
            step('C279 the public endpoint serves the app', status == '200', status)
        delete(made['id'])
    for label, arguments, words in (('no origin', '%s-n' % name, 'port|from|origin'), ('a bad name', "'bad name!' --port 8765", 'name'),
                                    ('a malformed --from', '%s-b --from ftp://127.0.0.1:1' % name, 'from|ftp|scheme|protocol')):
        code, _, both = pb('tunnel create %s' % arguments)
        step('C279 %s is refused with a short reason' % label, code != 0 and re.search(words, both, re.I) and not re.search(r'panic|goroutine|Traceback', both), both)
    code, out, both = pb('tunnel create --help')
    step('C279 --help lists --port, --from, --private, --duration', code == 0 and all(f in out for f in ('--port', '--from', '--private', '--duration')), both)
    return line_verdict('tunnel-create', 0, '\n'.join(steps), 'pb tunnel create: receipts, public and private, refusals in plain words', 7)


ORIGIN_SCENARIOS = ('headers', 'keepalive', 'ws', 'tls', 'grpc', 'failures')


def origin_cells(report):
    """tunnel_origin_rows.py's --out report (one JSON object, cells with result ok|FAILED): (cells, failed ones, scenarios that never ran)."""
    try:
        cells = json.loads(report)['cells']
    except (ValueError, KeyError, TypeError):
        cells = []
    failed = ['%s/%s (%s)' % (c.get('scenario'), c.get('cell'), str(c.get('detail', ''))[:110]) for c in cells if c.get('result') != 'ok']
    return cells, failed, sorted(set(ORIGIN_SCENARIOS) - {c.get('scenario') for c in cells})


def tunnel_origin(m):
    _, _, [(name, _, prefix)] = solo(m, ['torigin'])
    docker_exec(fx.HOST, name, 'apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq python3-venv >/dev/null', timeout=600)
    code, text = fx.on_machine(prefix, 'tunnel_origin_rows.py', ['--pb', PB, '--out', 'tor.jsonl'], 2700)
    rows = m.at(prefix, 'cat d/tor.jsonl 2>/dev/null || true', check=False).stdout
    log = fx.keep('tunnel-origin', text + '\n' + rows)
    cells, failed, missing = origin_cells(rows)
    if code == 0 and cells and not failed and not missing:
        return True, 'origin TLS modes with a pinned origin, gRPC with trailers, quiet WebSocket, keep-alive and failure pages; %d/%d cells, log %s' % (len(cells), len(cells), log)
    return False, '%d/%d cells failed (exit %d, missing %s): %s log %s' % (len(failed), len(cells), code, missing, '; '.join(failed)[:230] or ' '.join(text.split())[-230:], log)


def tunnel_origin_proxies(m):
    _, _, [(name, _, _)] = solo(m, ['tprox'], names=['fx-small'])
    out = str(Path(fx.tempfile.gettempdir()) / ('otm-%s.json' % m.run_id))
    m.callback(lambda: Path(out).unlink(missing_ok=True))
    code, _, text = script(['origin_tunnel_matrix.py', '--host', fx.HOST, '--container', name, '--pb', PB, '--out', out], 2700)
    log = fx.keep('tunnel-origin-proxies', text + '\n' + (Path(out).read_text() if Path(out).exists() else ''))
    return (code == 0), '%s log %s' % ('every origin proxy cell passed' if code == 0 else ' '.join(text.split())[-300:], log)


CHECKS = {'policy-cli': policy_cli, 'policy-token': policy_token, 'machine-tags': machine_tags, 'policy-team': policy_team,
          'tunnel-ip-rules': tunnel_ip_rules, 'preview-ip-rules': preview_ip_rules, 'tunnel-route-rules': tunnel_route_rules,
          'tunnel-create': tunnel_create, 'tunnel-origin': tunnel_origin, 'tunnel-origin-proxies': tunnel_origin_proxies}
