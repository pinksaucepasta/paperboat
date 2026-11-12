#!/usr/bin/env python3
"""X-21: team sharing across OSes. A machine of OS A (account OWNER, shared into a team) used by a member (account MEMBER)
from a machine of OS B, for every ordered pair A>B.

  xplat_team.py [--pairs L>M,M>L,...] [--only visible,exec,terminal,files,private,end]

Two fleets of the same shape, one per account (each: L = Docker container on XPLAT_L_HOST, M = macpool runner, W = winpool runner):
  owner:  XPLAT_L_CT, XPLAT_MAC_SSH, SMOKE_WINDOWS_SSH, names `<XPLAT_PREFIX>-l|-m|-w`; its L machine's pb is signed in as OWNER
  member: XPE_MEMBER_L_CT, XPE_MEMBER_MAC_SSH, XPE_MEMBER_WIN_SSH, names `<XPE_MEMBER_PREFIX>l|m|w` (xpe-bl) (paired to MEMBER)
  XPE_TEAM            a paid team OWNER owns with MEMBER as an active member (final_fixtures.py)
  XPE_OWNER_CTL_CT    Docker container of a Linux pb signed in as OWNER with its token in a file (the team setting `allow_private_access`)
Results: results/xplat-team-<stamp>.json with `X-21:<step>:A>B`. Nothing secret is printed; every grant, setting and share made is undone.
"""
import json
import os
import subprocess
import sys
import time
from contextlib import contextmanager
from pathlib import Path

HERE = Path(__file__).parent
sys.path.insert(0, str(HERE))
import xplat_access  # noqa: E402
import xplat_fleet as fleet  # noqa: E402
import xplat_send  # noqa: E402
import xplat_term  # noqa: E402

results = {}
TEAM = os.environ.get('XPE_TEAM', '')
CAPS = 'terminal,exec,files,private_access,preview_manage,tunnel_manage'
END_SECONDS = 30  # how long a revoked grant may keep a session alive (the documented bound of the CLI: 15 s on ssh, 30 s overall)
OWNER_PREFIX, MEMBER_PREFIX = os.environ.get('XPLAT_PREFIX', 'xpe'), os.environ.get('XPE_MEMBER_PREFIX', 'xpe-b')


def fleets():
    owner = {'L': fleet.linux(OWNER_PREFIX + '-l'), 'M': fleet.mac(OWNER_PREFIX + '-m'), 'W': fleet.windows(OWNER_PREFIX + '-w')}
    saved = fleet.L_CT, os.environ.get('XPLAT_MAC_SSH'), os.environ.get('SMOKE_WINDOWS_SSH')
    try:
        fleet.L_CT = os.environ['XPE_MEMBER_L_CT']
        os.environ['XPLAT_MAC_SSH'] = os.environ['XPE_MEMBER_MAC_SSH']
        member = {'L': fleet.linux(MEMBER_PREFIX + 'l'), 'M': fleet.mac(MEMBER_PREFIX + 'm'),
                  'W': fleet.Windows(MEMBER_PREFIX + 'w', target=os.environ['XPE_MEMBER_WIN_SSH'])}
    finally:
        fleet.L_CT = saved[0]
        os.environ['XPLAT_MAC_SSH'] = saved[1]
    return owner, member


@contextmanager
def as_host(dev):
    """The terminal drivers read the Docker container / Mac host from module state when they spawn: point them at `dev`'s host."""
    saved = fleet.L_CT, os.environ.get('XPLAT_MAC_SSH')
    if dev.os == 'linux':
        fleet.L_CT = dev.prefix[-1]
    elif dev.os == 'darwin':
        os.environ['XPLAT_MAC_SSH'] = dev.prefix[-1]
    try:
        yield
    finally:
        fleet.L_CT = saved[0]
        os.environ['XPLAT_MAC_SSH'] = saved[1] or ''


class Team:
    def __init__(self, admin, setter):
        self.admin, self.setter = admin, setter  # an owner-account pb, and a Linux device whose pb session of the owner keeps its token in a file
        self.id = TEAM

    def generation(self):
        return str(fleet.json_pb(self.admin, 'team', 'get', self.id)['generation'])

    def admin_pb(self, *args):
        done = self.admin.pb(*args, '--json', timeout=120)
        assert done.rc == 0, 'owner pb %s: %s' % (' '.join(args[:3]), (done.text + done.errtext)[-300:])
        return json.loads(done.text)

    def member_account(self):
        owner = fleet.json_pb(self.admin, 'auth', 'status')['account']['id']
        members = fleet.json_pb(self.admin, 'team', 'get', self.id)['members']
        return next(m['account_id'] for m in members if m['active'] and m['account_id'] != owner)

    def share(self, machine_id, caps=CAPS):
        shared = any(x['machine_id'] == machine_id and x['active'] for x in fleet.json_pb(self.admin, 'team', 'get', self.id).get('machines', []))
        if not shared:
            self.admin_pb('team', 'machine', 'share', self.id, machine_id, '--generation', self.generation())
        self.admin_pb('team', 'machine', 'grant', self.id, machine_id, '--member', self.member_account(), '--capability', caps,
                      '--generation', self.generation())

    def revoke(self, machine_id, caps=CAPS):
        self.admin_pb('team', 'machine', 'grant', self.id, machine_id, '--member', self.member_account(), '--capability', caps,
                      '--active=false', '--generation', self.generation())

    def unshare(self, machine_id):
        self.admin_pb('team', 'machine', 'unshare', self.id, machine_id, '--generation', self.generation())

    def setting(self, on):
        """The team's allow_private_access, set with the owner's CLI session (private_access_permission.SETTING, as the F9 check does)."""
        import private_access_permission
        self.setter.pb('machine', 'list', '--json')  # the access token lives minutes: any call refreshes the secret the script reads
        done = self.setter.run(['python3', '-c', private_access_permission.SETTING, self.id, 'on' if on else 'off'], timeout=120)
        assert done.rc == 0, 'team setting: ' + (done.text + done.errtext)[-300:]


def machine_id(dev):
    return next(m['id'] for m in fleet.json_pb(dev, 'machine', 'list', '--q', dev.name)['machines'] if m['alias'] == dev.name)


def py(dev):
    return 'python' if dev.os == 'windows' else 'python3'


# --- steps ------------------------------------------------------------------------------------------------------------
def permissions(team, o, m):
    got = [x for x in fleet.json_pb(m, '--workspace', team.id, 'machine', 'list')['machines'] if x['alias'] == o.name]
    assert got, 'the shared machine is not in the member\'s team list'
    return set(got[0]['permissions'])


def t_visible(team, o, m, oid):
    want = set(CAPS.split(','))
    off = permissions(team, o, m)
    assert off == want - {'private_access'}, 'with the team setting off the member sees %r, want the grant without private_access' % sorted(off)
    team.setting(True)
    try:
        time.sleep(3)
        on = permissions(team, o, m)
        assert on == want, 'with the team setting on the member sees %r, granted %s' % (sorted(on), CAPS)
    finally:
        team.setting(False)
    return 'the member sees %s with exactly the granted permissions (private_access only while the team setting is on)' % o.name


def t_exec(team, o, m, oid):
    done = m.pb('exec', o.name, '--', py(o), '-c', 'import sys,socket;print(socket.gethostname(), sys.argv[1:])', 'a b', '', 'q"x', timeout=120)
    assert done.rc == 0, 'exec: %s' % (done.text + done.errtext)[-300:]
    assert "['a b', '', 'q\"x']" in done.text, 'argv changed: %r' % done.text[-200:]
    return 'a member of OS %s ran a command on the %s machine with exact argv' % (m.os, o.os)


def t_terminal(team, o, m, oid):
    with as_host(m):
        term = xplat_term.Terminal(m, o, ['--workspace', team.id])
        try:
            term.ready()
            wide = 'λ船'
            command = {'unix': "a='%s'; printf 'U:%%s:E\\n' \"$a\"" % wide, 'cmd': 'set a=%s& call echo U:%%a%%:E' % wide,
                       'powershell': "$a='%s'; echo \"U:$a:E\"" % wide}[term.kind]
            term.say(command)
            term.expect('U:%s:E' % wide)
            return 'pb connect from the member machine started the target shell (%s) and echoed UTF-8' % term.kind
        finally:
            term.close()


def decide(admin, o, m, basename, action, seconds=120):
    """The owner side of a member's send: the exact pending Inbox request for `basename` from machine m to o is approved or declined."""
    deadline = time.time() + seconds
    while time.time() < deadline:
        requests = json.loads(admin.pb('inbox', 'requests', '--json').text)['data']['requests']
        match = [r for r in requests if r['status'] == 'pending' and [f['basename'] for f in r['files']] == [basename]
                 and r['destination_machine_id'] == machine_id(o)]
        if match:
            done = admin.pb('inbox', action, match[0]['request_id'], '--generation', str(match[0]['decision_generation']), '--json')
            assert done.rc == 0, 'inbox %s: %s' % (action, (done.text + done.errtext)[-200:])
            return
        time.sleep(1)
    raise AssertionError('the member\'s send never reached the owner as a pending Inbox request')


def send_with(team, o, m, name, content, action):
    """Member sends `content` as `name` to o; the owner decides `action`; returns the send's result."""
    import concurrent.futures
    path = xplat_send.sender_dir(m) + ('\\' if m.os == 'windows' else '/') + name
    m.write(path, content)
    try:
        with concurrent.futures.ThreadPoolExecutor(1) as pool:
            sending = pool.submit(m.pb, '--workspace', team.id, 'send', path, '--to', o.name, timeout=240)
            decide(team.admin, o, m, name, action)
            return sending.result()
    finally:
        m.remove(path)


def t_files(team, o, m, oid):
    """A member's send to a shared machine waits for the owner's decision on the exact request: approved bytes arrive in the Inbox,
    a declined send fails and nothing arrives."""
    import hashlib
    before = xplat_send.inbox_listing(o)
    try:
        content = b'xplat team send ' + os.urandom(48)
        done = send_with(team, o, m, 'xpe-team-ok.txt', content, 'approve')
        assert done.rc == 0, 'send: %s' % (done.text + done.errtext)[-300:]
        stored = [n for n, h in xplat_send.inbox_listing(o)['files'].items() if h == hashlib.sha256(content).hexdigest() and n not in before['files']]
        assert stored, 'the approved send succeeded but the bytes are not in the owner machine\'s Inbox'
        refused = send_with(team, o, m, 'xpe-team-no.txt', b'declined ' + os.urandom(16), 'decline')
        assert refused.rc != 0, 'a declined send reported success'
        names = list(xplat_send.inbox_listing(o)['files'])
        assert not any('xpe-team-no' in n for n in names), 'a declined send put a file in the Inbox'
        return 'approved send arrived with equal bytes in the %s Inbox; a declined send failed and left nothing (%s)' % (o.os, (refused.text + refused.errtext).strip()[-80:])
    finally:
        xplat_send.inbox_clean(o, before)


def t_private(team, o, m, oid):
    team.setting(True)
    try:
        return xplat_access.t_access(m, o)
    finally:
        team.setting(False)


def t_end(team, o, m, oid):
    """Revoking the grant ends the member's open terminal within END_SECONDS; the machine then disappears from the member's list; unshare
    does the same for a re-granted session."""
    out = []
    for how in ('revoke', 'unshare'):
        team.share(oid)
        with as_host(m):
            term = xplat_term.Terminal(m, o, ['--workspace', team.id])
        try:
            term.ready()
            (team.revoke if how == 'revoke' else team.unshare)(oid)
            started = time.time()
            term.say({'unix': 'sleep 120', 'cmd': 'ping -n 120 127.0.0.1 >nul', 'powershell': 'Start-Sleep 120'}[term.kind])
            while term.child.isalive() and time.time() - started < END_SECONDS + 15:
                time.sleep(1)
            ended = time.time() - started
            assert not term.child.isalive(), 'the member terminal was still open %ds after the %s' % (ended, how)
            assert ended <= END_SECONDS, 'the member terminal ended %ds after the %s (bound %ds)' % (ended, how, END_SECONDS)
            out.append('%s ended the open terminal in %ds' % (how, ended))
        finally:
            term.close()
    gone = [x for x in fleet.json_pb(m, '--workspace', team.id, 'machine', 'list')['machines'] if x['alias'] == o.name]
    assert not gone, 'the unshared machine is still in the member\'s team list'
    return '; '.join(out)


class Raw(xplat_term.Terminal):
    """A pb command with a terminal (`pb <tail>`) on src, driven like xplat_term.Terminal; dst is the machine whose shell it shows."""

    def __init__(self, src, dst, tail):
        import pexpect
        self.src, self.dst = src, dst
        argv = xplat_term.client_argv(src, [src.pb_path, *tail])
        self.child = pexpect.spawn(argv[0], argv[1:], encoding='utf-8', codec_errors='replace', timeout=90, dimensions=(30, 100))
        self.kind = 'unix' if dst.os != 'windows' else None
        self.unix = self.kind == 'unix'
        self.computer = ''


def t_session(team, o, m, oid):
    """A terminal session of the owner account on the shared machine, shared into the team: viewer reads and cannot type, interactive
    types, removing the member ends it; an unshared session cannot be joined."""
    import uuid
    name = 'xpe-share-' + uuid.uuid4().hex[:8]
    admin, member_id = team.admin, team.member_account()
    with as_host(admin):
        owner = Raw(admin, o, ['connect', o.name, '--name', name, '--status-bar', 'off'])
    joined = identifier = None
    shared = False
    try:
        owner.ready()
        old = uuid.uuid4().hex[:6]
        command, want = owner.echo(old)
        owner.say(command)
        owner.expect(want)
        sessions = fleet.json_pb(admin, 'session', 'list', o.name, '--all')['sessions']
        identifier = next(x['id'] for x in sessions if x['name'] == name)
        assert m.pb('--workspace', team.id, 'session', 'join', identifier, timeout=60).rc != 0, 'the member joined an unshared session'
        team.admin_pb('session', 'share', identifier, '--team', team.id, '--member', member_id, '--role', 'viewer')
        shared = True
        catalog = fleet.json_pb(m, '--workspace', team.id, 'session', 'shared', '--all')
        assert any(x['id'] == identifier for x in (catalog if isinstance(catalog, list) else catalog.get('sessions', []))), 'the shared catalog omits the session'
        with as_host(m):
            joined = Raw(m, o, ['--workspace', team.id, 'session', 'join', identifier])
        joined.kind, joined.unix = owner.kind, owner.unix
        joined.expect(want, timeout=90)
        blocked_command, blocked = owner.echo('blk' + old)
        joined.say(blocked_command)
        try:
            owner.child.expect(blocked, timeout=10)
            raise AssertionError('viewer input reached the shared shell')
        except Exception as error:  # noqa: BLE001
            if 'viewer input' in str(error):
                raise
        team.admin_pb('session', 'share', identifier, '--team', team.id, '--member', member_id, '--role', 'interactive')
        typed_command, typed = owner.echo('typ' + old)
        deadline = time.time() + 40
        while True:
            joined.say(typed_command)
            try:
                owner.child.expect(typed, timeout=6)
                break
            except Exception:  # noqa: BLE001
                assert time.time() < deadline, 'interactive input never reached the shared shell'
        team.admin_pb('session', 'remove', identifier, member_id)
        started = time.time()
        while joined.child.isalive() and time.time() - started < END_SECONDS + 15:
            time.sleep(1)
        assert not joined.child.isalive() and time.time() - started <= END_SECONDS, 'removing the member did not end the joined session in %ds' % END_SECONDS
        assert m.pb('--workspace', team.id, 'session', 'join', identifier, timeout=60).rc != 0, 'the removed member joined again'
        return 'viewer read and could not type, interactive typed, removal ended the join in %ds (member OS %s, target OS %s)' % (time.time() - started, m.os, o.os)
    finally:
        if shared:
            admin.pb('session', 'unshare', identifier, '--json')
        for term in (joined, owner):
            if term is not None:
                term.close()
        if identifier:
            for action in ('close', 'delete'):
                done = admin.pb('session', action, o.name, identifier, '--json')
                try:
                    admin.pb('session', action, o.name, identifier, '--confirm', json.loads(done.text)['data']['confirmation_token'], '--json')
                except (ValueError, KeyError):
                    pass


def origin_start(dev, port):
    """A tiny HTTP origin on the device's loopback; returns how to stop it."""
    if dev.os == 'windows':
        pid = dev.sh("(Start-Process python -ArgumentList '-m','http.server','%d','--bind','127.0.0.1' -WindowStyle Hidden -PassThru).Id" % port).text.strip()
        return lambda: dev.sh('Stop-Process -Id %s -Force -ErrorAction SilentlyContinue' % pid)
    pid = dev.sh('nohup python3 -m http.server %d --bind 127.0.0.1 >/dev/null 2>&1 & echo $!' % port).text.strip()
    return lambda: dev.run(['kill', pid])


def t_manage(team, o, m, oid):
    """Tunnel and preview management follows the machine grant: a team tunnel and a team preview made on the shared machine are listed and
    paused, resumed and stopped by the member; without tunnel_manage and preview_manage the member sees neither."""
    port = 18900 + sum(map(ord, o.name + m.name)) % 90
    name = 'xpe-mg-%s%s' % (o.os[:1], m.os[:1])
    stop_origin = origin_start(o, port)
    tunnel = preview = None
    try:
        made = o.pb('--workspace', team.id, 'tunnel', 'create', name, '--port', str(port), '--team', '--json', timeout=120)
        assert made.rc == 0, 'tunnel create: ' + (made.text + made.errtext)[-300:]
        tunnel = json.loads(made.text)['tunnel']['id']
        started = o.pb('--workspace', team.id, 'preview', str(port), '--team', '--background', '--ttl', '15m', '--json', timeout=180)
        assert started.rc == 0, 'preview: ' + (started.text + started.errtext)[-300:]
        value = json.loads(started.text)
        preview = value.get('id') or (value.get('preview') or {}).get('id')
        assert preview, 'no preview id in ' + started.text[-200:]

        def listed(kind, ident):
            done = m.pb('--workspace', team.id, kind, 'list', '--json', timeout=120)
            return done.rc == 0 and any(x.get('id') == ident for x in json.loads(done.text).get('items', json.loads(done.text).get('previews', [])))
        assert listed('tunnel', tunnel), 'the member does not list the team tunnel with tunnel_manage'
        assert listed('preview', preview), 'the member does not list the team preview with preview_manage'
        for action in ('pause', 'resume'):
            done = m.pb('--workspace', team.id, 'tunnel', action, tunnel, '--json', timeout=120)
            assert done.rc == 0, 'member tunnel %s: %s' % (action, (done.text + done.errtext)[-300:])
        done = m.pb('--workspace', team.id, 'preview', 'stop', preview, '--json', timeout=120)
        assert done.rc == 0, 'member preview stop: ' + (done.text + done.errtext)[-300:]
        team.revoke(oid, 'tunnel_manage,preview_manage')
        time.sleep(3)
        assert not listed('tunnel', tunnel) and not listed('preview', preview), 'the member still lists tunnel/preview after the management grant was revoked'
        return 'member (%s) listed, paused, resumed the team tunnel and stopped the team preview of the %s machine; revoking the grant hid both' % (m.os, o.os)
    finally:
        team.share(oid)  # restores the full grant for the next step
        if preview:
            o.pb('--workspace', team.id, 'preview', 'delete', preview, '--json')
        if tunnel:
            from xplat_tunnel import confirm_delete
            confirm_delete(o, tunnel)
        stop_origin()


STEPS = [('visible', t_visible), ('exec', t_exec), ('terminal', t_terminal), ('session', t_session), ('files', t_files), ('manage', t_manage), ('private', t_private), ('end', t_end)]


def check(name, pair, action):
    key = 'X-21:%s:%s' % (name, pair)
    started = time.time()
    try:
        detail = action()
        results[key] = {'status': 'PASS', 'detail': detail or ''}
        print('PASS %s %ds %s' % (key, time.time() - started, (detail or '')[:300]), flush=True)
    except Exception as error:  # noqa: BLE001
        results[key] = {'status': 'FAIL', 'detail': str(error)[:700]}
        print('FAIL %s %ds: %s' % (key, time.time() - started, str(error)[:700]), flush=True)


def main():
    argv = sys.argv[1:]
    pairs = argv[argv.index('--pairs') + 1].split(',') if '--pairs' in argv else ['L>M', 'M>L', 'L>W', 'W>L', 'M>W', 'W>M']
    only = set(argv[argv.index('--only') + 1].split(',')) if '--only' in argv else set()
    owner, member = fleets()
    saved = fleet.L_CT
    fleet.L_CT = os.environ['XPE_OWNER_CTL_CT']
    setter = fleet.linux('xpe-ctl')
    fleet.L_CT = saved
    team = Team(owner['L'], setter)
    for pair in pairs:
        a, b = pair.split('>')
        o, m = owner[a], member[b]
        oid = machine_id(o)
        team.share(oid)
        try:
            for name, step in STEPS:
                if only and name not in only:
                    continue
                check(name, pair, lambda: step(team, o, m, oid))
        finally:
            try:
                team.unshare(oid)
            except AssertionError:
                pass  # the end step already unshared it
    stamp = time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())
    out = HERE / 'results' / ('xplat-team-%s.json' % stamp)
    out.write_text(json.dumps({'check': 'xplat-team', 'pairs': pairs, 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
