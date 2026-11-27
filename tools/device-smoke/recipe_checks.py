#!/usr/bin/env python3
"""Recipe checks (PLAN F26): CHECKLIST F26-01..04, F26-06..08 and X-47.

usage: recipe_checks.py ID[,ID...] [--expect-version V]
  recipe-lifecycle         F26-01 create, list, show, update, rotate, revoke on the Linux creator; the token is shown once.
  recipe-errors            F26-07 bad file (line:col, did-you-mean), duplicate, vault locked; on a machine: malformed token, no root,
                           replaced, expired and revoked token.
  recipe-vaults            F26-04 a missing vault is named and can be created; vault reads with nobody online; a new value arrives;
                           rotate keeps a running machine reading; revoke drops only that recipe, without a new vault key.
  recipe-lifetimes         F26-06 max_machines, expires, machine_lifetime, remove_after_offline; rotate keeps the machine. ~10 min.
  recipe-api-tokens        F26-08 API tokens work next to recipes (create, list, sign in with one, revoke).
  recipe-install:container F26-02, F26-03, F26-04, X-47: malformed PB_RECIPE stops, the printed one-liner pairs with no prompt, a rerun
  recipe-install:linux     is a no-op, tags at pairing, [reach] allow and refuse (both ways between two recipe machines), a vault
  recipe-install:macos     value read with the creator offline, reach update, revoke removes the machines. macos and windows then
  recipe-install:windows   become the creator: F26-01 there, and a recipe and vault value made there apply on Linux.

Every run uses its own throwaway console account (fixture_checks.Machines.account): the smoke account's ENV vault password never
leaves the controller, and deleting the account takes every recipe, vault, tag and machine of the run with it. The creator is a
paired Linux container on SMOKE_FIXTURE_HOST (build SMOKE_FIXTURE_PB); Linux targets and peers are containers there too.
The real machines are SMOKE_RECIPE_LINUX_SSH, SMOKE_RECIPE_MACOS_SSH and SMOKE_RECIPE_WINDOWS_SSH: throwaway runners with NO
Paperboat on them (a machine that has any is refused and not touched); each is left without it.
  recipe-apply:container   F26-05 one commented recipe file with every section (tags, reach, env, config, preview, tunnel) is applied
  recipe-apply:linux       once after pairing (one preview and one tunnel serving a local app, the pull-only config repository, the ENV
  recipe-apply:macos       hook) and not again after the daemon restarts (the apply runs again and creates nothing). The config
  recipe-apply:windows     repository is a public HTTPS one (SMOKE_RECIPE_CONFIG_URL, default github.com/octocat/Spoon-Knife).
With --expect-version a release version (YYYY.MM.DD.N) the one-liner installs that release (PB_VERSION) and it is checked.
Secrets travel on stdin only. Prints one JSON receipt line {"check", "status": PASS|FAIL, "detail", "seconds"} per ID; every step
of a check runs even after an earlier one failed. Exit 0 only when every ID passed.
"""
import contextlib
import json
import os
import re
import secrets
import shlex
import sys
import time

import fixture_checks as f
import recipe_devices as rd

PLATFORMS = ('container', 'linux', 'macos', 'windows')
CHECKS = (('recipe-lifecycle', 'recipe-errors', 'recipe-vaults', 'recipe-lifetimes', 'recipe-api-tokens')
          + tuple('recipe-install:' + p for p in PLATFORMS) + ('recipe-apply:container', ))
CONFIG_URL = 'https://github.com/octocat/Spoon-Knife.git'  # small, public, its branch is main, no sync manifest
SSH_SETTING = {p: 'SMOKE_RECIPE_%s_SSH' % p.upper() for p in ('linux', 'macos', 'windows')}
# the paired smoke machines: a recipe check installs and uninstalls Paperboat, so it never runs on one of them
PAIRED = ('SMOKE_LINUX_SSH', 'SMOKE_LINUX_PEER_SSH', 'SMOKE_MACOS_SSH', 'SMOKE_MACOS_VM_SSH', 'SMOKE_WINDOWS_SSH', 'SMOKE_MEMBER_SSH',
          'SMOKE_FIXTURE_HOST', 'SMOKE_AU4_HOST')
RELEASE = re.compile(r'20\d\d\.\d\d\.\d\d\.\d+')
NOT_REACHED = "This machine's recipe does not reach that machine or action"
SHORT_TOKEN = 'pbr_short'
MALFORMED = 'PB_RECIPE is not a recipe token. A token starts with pbr_ and is 67 characters; this value is %d.' % len(SHORT_TOKEN)
ALREADY = 'This machine is already paired; nothing to do.'
REMOVED = 'This machine was removed from Paperboat'
NOT_VALID = 'This recipe token is not valid: the recipe was revoked or the token was mistyped.'
REPLACED = 'This recipe token was replaced'
EXPIRED = 'This recipe token expired'
FULL = 'already has 1 of 1 machines.'  # the recipe's name comes before it
NEEDS_ROOT = 'Installing needs root or passwordless sudo'
LOCKED = 'your ENV keys are locked on this computer. Unlock them with pb env unlock'
SET_UP = re.compile(r'Set up (\S+) \((machine_[0-9a-f-]+)\)')


def need(condition, message):
    if not condition:
        raise AssertionError(rd.redact(str(message))[:600])


def ok(result, what, contains=()):
    need(result.code == 0, '%s exited %d: %s' % (what, result.code, result.tail()))
    for text in [contains] if isinstance(contains, str) else contains:
        need(text in result.text, '%s does not say %r: %s' % (what, text, result.tail()))
    return result


def refused(result, what, contains):
    need(result.code not in (0, 124, 127), '%s was not refused (exit %d): %s' % (what, result.code, result.tail()))
    flat = ' '.join(result.text.split())
    for text in [contains] if isinstance(contains, str) else contains:
        need(text in flat, '%s is refused without %r: %s' % (what, text, result.tail()))
    return result


def wait_for(what, probe, seconds, every=5):
    deadline = time.monotonic() + seconds
    while True:
        try:
            return probe()
        except AssertionError as error:
            if time.monotonic() >= deadline:
                raise AssertionError('%s not within %d s: %s' % (what, seconds, error)) from None
        time.sleep(every)


def token_parts(token):
    """(public id, secret) of a recipe token; the secret is registered for redaction."""
    need(rd.PB_RECIPE_RE.fullmatch(token or ''), 'not a recipe token')
    _, ident, secret = token.split('_', 2)
    rd.SECRETS.add(secret)
    return ident, secret


def shown_once(output):
    """The one token in the output of create or rotate."""
    found = rd.PB_RECIPE_RE.findall(output)
    need(len(found) == 1, 'the output shows %d tokens, not one' % len(found))
    token_parts(found[0])
    return found[0]


def confirm_code(output, verb, name):
    found = re.search(r'pb recipe %s %s --confirm ([A-Z0-9]+)' % (verb, re.escape(name)), output)
    need(found, 'no "%s with: ... --confirm CODE" line: %s' % (verb, output[-300:]))
    return found.group(1)


def recipe_file(spec, comment='E2E recipe check'):
    return '// %s\n%s\n' % (comment, json.dumps(spec, indent=2))


class Steps:
    """The steps of one check. A failed step is recorded and the next one still runs."""

    def __init__(self, check, log=None):
        self.check, self.passed, self.failed, self.notes, self.log = check, [], [], [], log if log is not None else []

    def say(self, text):
        line = '%s %s: %s' % (time.strftime('%H:%M:%SZ', time.gmtime()), self.check, rd.redact(text))
        self.log.append(line)
        print(line, file=sys.stderr, flush=True)

    @contextlib.contextmanager
    def step(self, name):
        began = time.monotonic()
        try:
            yield
        except Exception as error:  # noqa: BLE001 every failure is collected, never fail fast
            text = '%s: %s' % (type(error).__name__, error) if not isinstance(error, AssertionError) else str(error)
            self.failed.append((name, rd.redact(text)))
            self.say('FAIL %s (%d s): %s' % (name, time.monotonic() - began, text))
        else:
            self.passed.append(name)
            self.say('ok %s (%d s)' % (name, time.monotonic() - began))

    def result(self):
        if self.failed:
            names = ', '.join(name for name, _ in self.failed)
            return False, '%d of %d steps failed: %s; first: %s' % (len(self.failed), len(self.failed) + len(self.passed), names, self.failed[0][1][:200])
        if not self.passed:
            return False, 'no step ran'
        return True, '; '.join(self.notes + ['%d steps: %s' % (len(self.passed), ', '.join(self.passed))])


class Session:
    """One run's throwaway account and its creator container (with an ENV vault)."""

    def __init__(self, machines):
        self.m, self.run_id = machines, secrets.token_hex(2)
        self.day = time.strftime('%m%d')
        self.password = secrets.token_hex(12) + 'Aa1!'
        rd.SECRETS.add(self.password)
        self.vault, self.marker = 'rcp-vault', 'rcpvalue' + secrets.token_hex(6)
        _, self.state = machines.account('rcp')
        self.creator_name = self.name('creator')
        machines.machine('rcp', state=self.state, name=self.creator_name)
        self.creator = rd.Posix('container', f.HOST, self.creator_name)
        self._vault_ready = False

    def name(self, role):
        return 'test-%s-%s-%s-%s' % (self.day, f.ENV.get('SMOKE_RECIPE_NAME_TAG', 'rcp'), role, self.run_id)

    def vault_ready(self):
        if not self._vault_ready:
            wait_for('pb env init on the creator', lambda: ok(self.creator.pb('env', 'init', '--password-stdin', stdin=self.password), 'pb env init'), 240, 10)
            ok(self.creator.pb('env', 'vault', 'create', self.vault), 'pb env vault create')
            self.set_value('RCP_SECRET', self.marker)
            self._vault_ready = True
        else:
            self.unlock()

    def unlock(self, device=None):
        return ok((device or self.creator).pb('env', 'unlock', '--password-stdin', stdin=self.password), 'pb env unlock')

    def set_value(self, key, value, device=None):
        ok((device or self.creator).pb('env', 'set', key, '--vault', self.vault, '--value-stdin', stdin=value), 'pb env set --vault')

    def enrollment_token(self):
        token = self.m.node('mint_enrollment_token.mjs', state=self.state).stdout.strip()
        need(token, 'no enrollment token')
        rd.SECRETS.add(token)
        return token


class Check:
    """One check: its recipes (revoked at its end) and devices (removed at its end)."""

    def __init__(self, session, steps, stack, expect=''):
        self.s, self.steps, self.stack = session, steps, stack
        self.pin = expect if RELEASE.fullmatch(expect or '') else None
        self.live = []  # (creator device, recipe name) not yet revoked
        self.tag = 'rcp%s%s' % (session.run_id, secrets.token_hex(2))

    # -- recipes
    def write(self, creator, name, spec, comment='E2E recipe check'):
        return creator.write(name + '.jsonc', recipe_file(spec, comment))

    def create(self, name, spec, creator=None):
        """(token, installer URL for Linux and macOS, installer URL for Windows)."""
        creator = creator or self.s.creator
        path = self.write(creator, name, spec)
        self.live.append((creator, name))
        result = creator.pb('recipe', 'create', '-f', path, '--json')
        found = rd.PB_RECIPE_RE.findall(result.text)
        for token in found:
            token_parts(token)
        data = rd.json_data(result)
        token_parts(data.get('token', ''))
        return data['token'], rd.installer_url(data['install']['linux_macos']), rd.installer_url(data['install']['windows'], windows=True)

    def update(self, name, spec, creator=None, comment='E2E recipe check'):
        creator = creator or self.s.creator
        return ok(creator.pb('recipe', 'update', '-f', self.write(creator, name, spec, comment)), 'pb recipe update').out

    def confirmed(self, verb, name, creator=None):
        """rotate or revoke: the printed preview, then the confirmed command. Returns (preview, result)."""
        creator = creator or self.s.creator
        preview = ok(creator.pb('recipe', verb, name), 'pb recipe %s preview' % verb).out
        code = confirm_code(preview, verb, name)
        result = ok(creator.pb('recipe', verb, name, '--confirm', code), 'pb recipe %s --confirm' % verb)
        if verb == 'revoke' and (creator, name) in self.live:
            self.live.remove((creator, name))
        return preview, result

    def listed(self, creator=None):
        data = rd.json_data((creator or self.s.creator).pb('recipe', 'list', '--json'))
        return {r['name']: r for r in data['recipes'] or []}

    def shown(self, name, creator=None):
        return rd.json_data((creator or self.s.creator).pb('recipe', 'show', name, '--json'))

    def revoke_all(self):
        for creator, name in list(reversed(self.live)):
            try:
                if name in self.listed(creator):
                    self.confirmed('revoke', name, creator)
                elif (creator, name) in self.live:
                    self.live.remove((creator, name))
            except Exception as error:  # noqa: BLE001 try the others; the account deletion still takes it
                self.steps.failed.append(('cleanup:' + name, rd.redact(str(error))[:200]))
                if creator is not self.s.creator:  # the creator container sees the same account
                    with contextlib.suppress(Exception):
                        self.confirmed('revoke', name, self.s.creator)

    # -- machines
    def machines(self):
        data = rd.json_data(self.s.creator.pb('machine', 'list', '--json'))
        return data['machines'] or []

    def named(self, name):
        return [m for m in self.machines() if m.get('alias') == name]

    def gone(self, *names):
        left = [m['alias'] for m in self.machines() if m.get('alias') in names]
        need(not left, 'still listed: %s' % ', '.join(left))

    def bare(self, role):
        """(name, device) of a new container with no Paperboat."""
        name = self.s.name(role) + '-' + secrets.token_hex(1)
        down = [str(f.HERE / 'fixture_machine.sh'), 'down', f.HOST, name]
        self.stack.callback(f.run, down, check=False, env=self.s.m.env, timeout=300)
        made = f.run([str(f.HERE / 'fixture_machine.sh'), 'bare', f.HOST, name], env=self.s.m.env, timeout=900, check=False)
        need(made.returncode == 0, 'container %s: %s' % (name, made.stderr.strip()[-300:]))
        return name, rd.Posix('container', f.HOST, name)

    @contextlib.contextmanager
    def nobody_online(self):
        """The creator, the account's only other computer, is frozen: what a recipe machine reads now it reads on its own."""
        f.remote('%s pause %s >/dev/null' % (f.DOCKER, self.s.creator_name))
        try:
            yield
        finally:
            f.remote('%s unpause %s >/dev/null' % (f.DOCKER, self.s.creator_name))

    def install(self, device, url, token, name, any_name=False):
        """The printed one-liner on a clean or unpaired device; returns the machine id it set up. any_name: return
        (machine id, the name it got) and leave judging the name to the caller."""
        result = ok(device.one_liner(url, 'PB_RECIPE', token, name, self.pin), 'the recipe one-liner', 'Pairing this machine from recipe ')
        need('Waiting for approval' not in result.text and 'sign in and approve' not in result.text, 'the recipe install asked for a sign-in: ' + result.tail())
        found = SET_UP.search(result.text)
        need(found and (any_name or found.group(1) == name), 'no "Set up %s (machine_…)" line: %s' % (name, result.tail()))
        return (found.group(2), found.group(1)) if any_name else found.group(2)

    def refuse_install(self, device, url, token, name, *texts):
        return refused(device.one_liner(url, 'PB_RECIPE', token, name, self.pin), 'the recipe one-liner', list(texts))

    def read(self, device, key, expected, seconds=240):
        def probe():
            done = device.pb('env', 'run', '--', *device.print_env_args(key), timeout=90)
            need(done.code == 0 and done.out.strip() == expected, 'pb env run printed something else (exit %d): %s' % (done.code, done.err.strip()[-250:] or '<other value>'))
        wait_for('the vault value %s on the %s machine' % (key, device.os), probe, seconds)

    def unreadable(self, device, value, seconds=180):
        def probe():
            done = device.pb('env', 'run', '--', *device.print_env_args('RCP_SECRET'), timeout=90)
            need(value not in done.text, 'the removed machine still reads the vault value')
        wait_for('the vault value gone', probe, seconds)

    def exec_hostname(self, source, name, hostname, seconds=180):
        def probe():
            done = source.pb('exec', name, '--', 'hostname', timeout=90)
            need(done.code == 0 and hostname.lower() in done.out.lower().split(), 'pb exec %s: exit %d %s' % (name, done.code, done.tail(200)))
        wait_for('pb exec %s from the %s machine' % (name, source.os), probe, seconds)

    def exec_refused(self, source, name, seconds=180):
        def probe():
            refused(source.pb('exec', name, '--', 'hostname', timeout=90), 'pb exec %s outside the reach' % name, NOT_REACHED)
        wait_for('pb exec %s refused' % name, probe, seconds)


# ------------------------------------------------------------------ F26-01
def lifecycle(check, creator, prefix=''):
    steps, name = check.steps, 'rcpl%s%s' % (creator.os[0], check.tag[3:])
    spec = {'tags': [name], 'max_machines': 3, 'expires': '1d'}
    comment = 'lifecycle on ' + creator.os
    state = {}
    with steps.step(prefix + 'create'):
        path = check.write(creator, name, spec, comment)
        check.live.append((creator, name))
        result = ok(creator.pb('recipe', 'create', '-f', path), 'pb recipe create',
                    ['Recipe %s created in workspace personal.' % name, 'Token (shown once; save it as the secret PB_RECIPE):', 'Install, with PB_RECIPE set:'])
        state['token'] = shown_once(result.text)
        lines = {k: v for k, v in re.findall(r'^\s*(Linux, macOS|Windows)\s{2,}(.+?)\s*$', result.out, re.M)}
        rd.installer_url(lines.get('Linux, macOS', ''))
        rd.installer_url(lines.get('Windows', ''), windows=True)
    ident, secret = token_parts(state['token']) if 'token' in state else ('', '')
    with steps.step(prefix + 'list'):
        row = check.listed(creator).get(name)
        need(row, 'the recipe is not listed')
        need((row['revision'], row['machines'], row.get('max_machines'), row.get('rotated_at')) == (1, 0, 3, None), 'list shows %r' % {k: row.get(k) for k in ('revision', 'machines', 'max_machines', 'rotated_at')})
        text = ok(creator.pb('recipe', 'list'), 'pb recipe list').out
        need(re.search(r'^NAME\s+TAGS\s+MACHINES\s+EXPIRES\s+LAST USED', text, re.M) and re.search(r'^%s\s+%s\s+0 / 3\s' % (name, name), text, re.M), 'list table: ' + text[-300:])
    with steps.step(prefix + 'show'):
        text = ok(creator.pb('recipe', 'show', name), 'pb recipe show', ['Recipe %s, revision 1, token pbr_%s_' % (name, ident), '// ' + comment, 'Machines (0 of 3)']).out
        need(check.shown(name, creator)['recipe']['source'].replace('\r\n', '\n') == recipe_file(spec, comment), 'show does not return the file as created: ' + text[-200:])
    with steps.step(prefix + 'token-shown-once'):
        need(secret, 'no token from create')
        for args in (('list',), ('list', '--json'), ('show', name), ('show', name, '--json')):
            done = ok(creator.pb('recipe', *args), 'pb recipe ' + args[0])
            need(secret not in done.text, 'pb recipe %s shows the token again' % ' '.join(args))
            need('pbr_%s_' % ident in done.text or args == ('list',), 'pb recipe %s does not show the public token id' % ' '.join(args))
    with steps.step(prefix + 'update'):
        said = check.update(name, spec, creator, comment)
        need('Recipe %s is unchanged (revision 1).' % name in said, 'an update with the same file said: ' + said[-200:])
        said = check.update(name, dict(spec, max_machines=4), creator, comment)
        need('Recipe %s updated to revision 2:' % name in said and 'max_machines' in said, 'update said: ' + said[-200:])
        row = check.listed(creator)[name]
        need((row['revision'], row.get('max_machines')) == (2, 4), 'after update: revision %s max_machines %s' % (row['revision'], row.get('max_machines')))
    with steps.step(prefix + 'rotate'):
        preview = ok(creator.pb('recipe', 'rotate', name), 'pb recipe rotate', ['Rotating %s gives it a new token.' % name, 'The old token stops working at once.']).out
        code = confirm_code(preview, 'rotate', name)
        wrong = 'AAAAAA' if code != 'AAAAAA' else 'BBBBBB'
        again = creator.pb('recipe', 'rotate', name, '--confirm', wrong)
        need(not rd.PB_RECIPE_RE.search(again.text) and not check.listed(creator)[name].get('rotated_at'), 'a wrong confirmation code rotated the token')
        result = ok(creator.pb('recipe', 'rotate', name, '--confirm', code), 'pb recipe rotate --confirm', 'Recipe %s rotated. New token (shown once):' % name)
        new = shown_once(result.text)
        need(new != state.get('token') and token_parts(new)[0] == ident, 'rotate must keep the id and change the secret')
        need(check.listed(creator)[name].get('rotated_at'), 'list does not record the rotation')
        need(token_parts(new)[1] not in creator.pb('recipe', 'show', name, '--json').text, 'show prints the rotated token')
    with steps.step(prefix + 'revoke'):
        preview = ok(creator.pb('recipe', 'revoke', name), 'pb recipe revoke', ['Revoking %s:' % name, 'its token stops working']).out
        need(name in check.listed(creator), 'a revoke without --confirm revoked the recipe')
        result = ok(creator.pb('recipe', 'revoke', name, '--confirm', confirm_code(preview, 'revoke', name)), 'pb recipe revoke --confirm')
        need('Recipe %s revoked. 0 machines removed.' % name in result.out, 'revoke said: ' + result.tail())
        check.live.remove((creator, name))
        need(name not in check.listed(creator), 'the revoked recipe is still listed')
        need(creator.pb('recipe', 'show', name).code != 0, 'the revoked recipe can still be shown')


def check_lifecycle(check):
    lifecycle(check, check.s.creator)


# ------------------------------------------------------------------ F26-07
def check_errors(check):
    steps, s = check.steps, check.s
    creator = s.creator
    with steps.step('bad-file-did-you-mean'):
        path = creator.write(check.tag + 'bad.jsonc', '{ "tag": ["ci"] }\n')
        result = refused(creator.pb('recipe', 'create', '-f', path), 'a file with an unknown key', 'unknown key "tag". Did you mean "tags"?')
        need(re.search(r'bad\.jsonc:1:\d+: ', result.text), 'no file:line:column: ' + result.tail())
    with steps.step('bad-value'):
        path = creator.write(check.tag + 'val.jsonc', '{\n  "max_machines": "two"\n}\n')
        result = refused(creator.pb('recipe', 'create', '-f', path), 'a wrong value', 'max_machines')
        need(re.search(r'val\.jsonc:2:\d+: ', result.text), 'no file:line:column: ' + result.tail())
    name = check.tag + 'e'
    spec = {'tags': [name], 'max_machines': 3, 'expires': '1d'}
    got = {}
    with steps.step('duplicate-name'):
        got['token'], got['url'], _ = check.create(name, spec)
        refused(creator.pb('recipe', 'create', '-f', check.write(creator, name, spec)), 'a second recipe of the name',
                ['A recipe with this name already exists.', 'pb recipe update -f'])
    with steps.step('vault-locked'):
        s.vault_ready()
        locked = check.tag + 'v'
        path = check.write(creator, locked, {'env': {'vaults': [s.vault]}, 'expires': '1d'})
        ok(creator.pb('env', 'lock'), 'pb env lock')
        try:
            refused(creator.pb('recipe', 'create', '-f', path), 'a vault recipe with ENV locked', ['reads vault ' + s.vault, LOCKED])
            need(locked not in check.listed(), 'the refused recipe was created')
        finally:
            s.unlock()
    device = {}
    with steps.step('malformed-token'):
        need(got, 'no recipe to install from')
        device['name'], device['d'] = check.bare('errors')
        result = refused(device['d'].one_liner(got['url'], 'PB_RECIPE', SHORT_TOKEN, device['name']), 'a malformed PB_RECIPE', MALFORMED)
        need(result.code == 2 and not device['d'].present(), 'the malformed token did not stop before anything was installed: ' + result.tail())
    with steps.step('no-root'):
        need(got, 'no recipe to install from')
        host_name, _ = check.bare('noroot')
        f.remote('%s exec -u root %s useradd -m -s /bin/bash rcpnoroot' % (f.DOCKER, host_name))
        check.refuse_install(rd.Posix('container', f.HOST, host_name, user='rcpnoroot'), got['url'], got['token'], host_name, NEEDS_ROOT)
        check.gone(host_name)
    with steps.step('replaced-token'):
        need(got and device, 'no recipe or machine')
        _, result = check.confirmed('rotate', name)
        new = shown_once(result.text)
        check.refuse_install(device['d'], got['url'], got['token'], device['name'], REPLACED, 'pb recipe rotate %s' % name)
        check.gone(device['name'])
        got['token'] = new
    with steps.step('revoked-token'):
        need(got and device, 'no recipe or machine')
        check.confirmed('revoke', name)
        check.refuse_install(device['d'], got['url'], got['token'], device['name'], NOT_VALID)
        check.gone(device['name'])
    with steps.step('expired-token'):
        need(got and device, 'no recipe or machine')
        short = check.tag + 'x'
        token, url, _ = check.create(short, {'tags': [short], 'expires': '1m'})
        time.sleep(75)
        check.refuse_install(device['d'], url, token, device['name'], EXPIRED, 'pb recipe rotate %s' % short)
        check.gone(device['name'])


# ------------------------------------------------------------------ F26-04
def check_vaults(check):
    steps, s = check.steps, check.s
    creator = s.creator
    with steps.step('vault-setup'):
        s.vault_ready()
    with steps.step('missing-vault-named-then-created'):
        # the console's "create the missing vault" offer went with its recipe editor (2026-11-21): the CLI names it
        missing, name = 'rcp-new-' + check.tag[3:], check.tag + 'm'
        spec = {'env': {'vaults': [missing]}, 'expires': '1d'}
        path = check.write(creator, name, spec)
        refused(creator.pb('recipe', 'create', '-f', path), 'a recipe naming a missing vault', 'vault "%s" does not exist' % missing)
        need(name not in check.listed(), 'the refused recipe was created')
        ok(creator.pb('env', 'vault', 'create', missing), 'pb env vault create')
        check.create(name, spec)
        refused(creator.pb('env', 'vault', 'delete', missing), 'deleting a vault a recipe reads', 'A recipe reads this vault.')
        check.confirmed('revoke', name)
        ok(creator.pb('env', 'vault', 'delete', missing), 'pb env vault delete')
    recipes, installed = {}, {}
    for label in ('a', 'b'):
        with steps.step('install-' + label):
            name = check.tag + label
            token, url, _ = check.create(name, {'tags': [name], 'env': {'vaults': [s.vault]}, 'expires': '1d'})
            host, device = check.bare('vault' + label)
            check.install(device, url, token, host)
            recipes[label], installed[label] = name, (host, device)
    with steps.step('read-with-nobody-online'):
        need(len(installed) == 2, 'both machines are needed')
        ok(creator.pb('env', 'lock'), 'pb env lock')
        try:
            with check.nobody_online():
                for _, device in installed.values():
                    check.read(device, 'RCP_SECRET', s.marker)
        finally:
            s.unlock()
    second = s.marker + 'two'
    with steps.step('new-value-arrives'):
        need(installed, 'no recipe machine')
        s.set_value('RCP_SECRET', second)
        for _, device in installed.values():
            check.read(device, 'RCP_SECRET', second)
    third = s.marker + 'three'
    with steps.step('rotate-keeps-the-machine-reading'):
        need('a' in installed, 'no machine of recipe a')
        s.unlock()
        check.confirmed('rotate', recipes['a'])
        s.set_value('RCP_SECRET', third)
        check.read(installed['a'][1], 'RCP_SECRET', third)
    with steps.step('revoke-without-rekey'):
        need(len(installed) == 2, 'both machines are needed')

        def vault():
            rows = [v for v in rd.json_data(creator.pb('env', 'vault', 'list', '--json'))['vaults'] if v['name'] == s.vault]
            need(len(rows) == 1, 'the vault is not listed once')
            return rows[0]
        before = vault()
        _, result = check.confirmed('revoke', recipes['b'])
        need('1 machine removed' in result.out, 'revoke said: ' + result.tail())
        after = vault()
        need(after['key_epoch'] == before['key_epoch'], 'revoke gave the vault a new key (epoch %s -> %s)' % (before['key_epoch'], after['key_epoch']))
        need(recipes['b'] not in (after.get('recipes') or []) and recipes['a'] in (after.get('recipes') or []), 'vault readers after revoke: %s' % after.get('recipes'))
        check.read(installed['a'][1], 'RCP_SECRET', third)
        check.unreadable(installed['b'][1], third)
        wait_for('the machine of the revoked recipe leaving the list', lambda: check.gone(installed['b'][0]), 120)
    with steps.step('no-value-in-metadata'):
        need(recipes, 'no recipe')
        for args in (('env', 'vault', 'list', '--json'), ('recipe', 'show', recipes['a'], '--json'), ('recipe', 'list', '--json')):
            done = ok(creator.pb(*args), 'pb ' + ' '.join(args[:3]))
            need(s.marker not in done.text, 'pb %s carries a vault value' % ' '.join(args))
    with contextlib.suppress(Exception):
        s.unlock()
        s.set_value('RCP_SECRET', s.marker)


# ------------------------------------------------------------------ F26-06
def check_lifetimes(check):
    steps = check.steps
    recipes, hosts, at = {}, {}, {}
    with steps.step('containers'):
        for role in ('life', 'offline', 'expiry', 'late'):
            hosts[role] = check.bare(role)

    def made(label, extra):
        name = check.tag + label
        token, url, _ = check.create(name, dict({'tags': [name], 'expires': '1d'}, **extra))
        recipes[label] = {'name': name, 'token': token, 'url': url}
        return recipes[label]
    with steps.step('max-machines'):
        r = made('life', {'max_machines': 1, 'machine_lifetime': '5m'})
        host, device = hosts['life']
        check.install(device, r['url'], r['token'], host)
        at['life'] = time.monotonic()
        other, second = hosts['offline']
        check.refuse_install(second, r['url'], r['token'], other, FULL, 'raise max_machines')
        check.gone(other)
        need(check.shown(r['name'])['machines'][0].get('remove_at'), 'show gives the machine no removal time under machine_lifetime')
    with steps.step('remove-after-offline-and-rotate-keeps-the-machine'):
        r = made('off', {'remove_after_offline': '5m'})
        host, device = hosts['offline']
        check.install(device, r['url'], r['token'], host)
        check.confirmed('rotate', r['name'])
        ok(device.pb('machine', 'list'), 'pb on the machine after the rotation')
        wait_for('the machine online after the rotation', lambda: need([m for m in check.named(host) if m.get('online')], 'not online'), 120)
        f.remote('%s stop -t 30 %s >/dev/null' % (f.DOCKER, host), timeout=120)
        at['offline'] = time.monotonic()
    with steps.step('expiry-refuses-pairing-and-keeps-machines'):
        r = made('exp', {'expires': '3m'})
        host, device = hosts['expiry']
        began = time.monotonic()
        check.install(device, r['url'], r['token'], host)
        time.sleep(max(0, 195 - (time.monotonic() - began)))
        late, other = hosts['late']
        check.refuse_install(other, r['url'], r['token'], late, EXPIRED, 'pb recipe rotate %s' % r['name'])
        check.gone(late)
        need(check.named(host), 'expiry removed the machine paired before it')
        ok(device.pb('machine', 'list'), 'pb on the machine after its recipe expired')
    with steps.step('machine-lifetime-removes'):
        need('life' in at, 'the lifetime machine never paired')
        host, device = hosts['life']
        if time.monotonic() - at['life'] < 240:
            need(check.named(host), 'removed before its 5 minute lifetime')
        wait_for('removal after machine_lifetime=5m', lambda: check.gone(host), max(60, 480 - (time.monotonic() - at['life'])), 15)
        check.steps.notes.append('lifetime removal %d s after pairing' % (time.monotonic() - at['life']))
        need(time.monotonic() - at['life'] >= 270, 'removed %d s after pairing, before its lifetime' % (time.monotonic() - at['life']))
        check.refuse_install(device, recipes['life']['url'], recipes['life']['token'], host, REMOVED)
    with steps.step('remove-after-offline-removes'):
        need('offline' in at, 'the offline machine never paired')
        host, _ = hosts['offline']
        if time.monotonic() - at['offline'] < 240:
            need(check.named(host), 'removed before 5 minutes offline')
        wait_for('removal after remove_after_offline=5m', lambda: check.gone(host), max(60, 600 - (time.monotonic() - at['offline'])), 15)
        check.steps.notes.append('offline removal seen %d s after the stop' % (time.monotonic() - at['offline']))


# ------------------------------------------------------------------ F26-08
def check_api_tokens(check):
    steps, s = check.steps, check.s
    creator, label, got = s.creator, check.tag + 'at', {}
    with steps.step('api-token-create-list'):
        result = ok(creator.pb('api-token', 'create', label, '--expires', '1d'), 'pb api-token create', 'The secret below is shown once')
        secret = result.out.strip()
        need(re.fullmatch(r'pbat_\S+', secret), 'stdout is not the token alone')
        rd.SECRETS.add(secret)
        got['secret'] = secret
        for args in (('list',), ('list', '--json')):
            done = ok(creator.pb('api-token', *args), 'pb api-token list', label)
            need(secret not in done.text, 'pb api-token list shows the secret')
    with steps.step('recipes-next-to-api-tokens'):
        name = check.tag + 'k'
        check.create(name, {'tags': [name], 'expires': '1d'})
        need(name in check.listed(), 'the recipe is not listed next to an API token')
        check.confirmed('revoke', name)
        need(label in ok(creator.pb('api-token', 'list'), 'pb api-token list').out, 'revoking a recipe changed the API tokens')
    other = {}
    with steps.step('sign-in-with-api-token'):
        need(got, 'no API token')
        _, url, _ = check.create(check.tag + 'u', {'expires': '1d'})
        host, device = check.bare('apitoken')
        ok(device.sh('set -o pipefail; curl -fsSL %s | sh -s -- --no-setup 2>&1' % shlex.quote(url), timeout=600), 'install without setup')
        ok(device.pb('login', '--api-token', '-', stdin=got['secret']), 'pb login --api-token -', 'API token')
        ok(device.pb('recipe', 'list'), 'pb recipe list signed in with the API token', check.tag + 'u')
        other['device'] = device
    with steps.step('revoke-api-token'):
        need(got, 'no API token')
        ok(creator.pb('api-token', 'revoke', label, '--yes'), 'pb api-token revoke', 'Revoked API token')
        if other:
            wait_for('the revoked token session ending', lambda: need(other['device'].pb('recipe', 'list').code != 0, 'the session of the revoked token still works'), 120)


# ------------------------------------------------------------------ F26-02, F26-03, X-47
def target_device(check, platform):
    """(name, device) of the machine under test."""
    if platform == 'container':
        return check.bare('target')
    host = f.ENV.get(SSH_SETTING[platform], '')
    need(host, '%s is not set (a throwaway %s machine with no Paperboat)' % (SSH_SETTING[platform], platform))
    def machine(login):
        return login.rpartition('@')[2].lower()
    for key in PAIRED:
        need(machine(f.ENV.get(key, '')) != machine(host), '%s is the paired smoke machine or fixture host of %s: a recipe check never runs there' % (SSH_SETTING[platform], key))
    need('victus' not in host.lower(), 'a personal computer is never a recipe test machine')
    device = rd.Windows(host) if platform == 'windows' else rd.Posix(platform, host)
    return check.s.name(platform[:3]), device


def check_install(check, platform):
    steps, s = check.steps, check.s
    creator, tag = s.creator, check.tag
    with steps.step('vault-setup'):
        s.vault_ready()
    name, target = None, None
    with steps.step('target-has-no-paperboat'):
        candidate_name, candidate = target_device(check, platform)
        found = candidate.present() + candidate.hooks()
        need(not found, 'the %s machine already has Paperboat (%s): it is not touched; use a fresh runner' % (platform, ', '.join(found)[:200]))
        name, target = candidate_name, candidate
    if target is None:
        return  # nothing may run on a machine that is not a clean one
    # it was clean: whatever the installer leaves there is taken off again on every exit path (a container just goes)
    if platform != 'container':
        check.stack.callback(uninstall, steps, target)
    spec = {'tags': [tag], 'reach': [{'dst': ['tag:' + tag], 'allow': ['exec']}], 'env': {'vaults': [s.vault]}, 'max_machines': 3, 'expires': '1d'}
    recipe = {}
    with steps.step('recipe-create'):
        token, posix_url, windows_url = check.create(tag, spec)
        recipe.update(token=token, url=windows_url if target.os == 'windows' else posix_url, linux=posix_url)
    if not recipe:
        return
    with steps.step('malformed-token-stops'):
        result = refused(target.one_liner(recipe['url'], 'PB_RECIPE', SHORT_TOKEN, name), 'a malformed PB_RECIPE', MALFORMED)
        need(not target.present(), 'a malformed token left something installed: ' + result.tail())
    ids, peer = {}, {}
    with steps.step('lock-creator-keys'):
        ok(creator.pb('env', 'lock'), 'pb env lock')
    with check.nobody_online():
        with steps.step('peer-one-liner'):
            peer['name'], peer['device'] = check.bare('peer')
            ids[peer['name']] = check.install(peer['device'], recipe['linux'], recipe['token'], peer['name'])
        wanted, got_name = name, None
        with steps.step('one-liner-no-prompt'):
            machine_id, got_name = check.install(target, recipe['url'], recipe['token'], name, any_name=True)
            name = got_name  # the later steps follow the machine by the name it has
            ids[name] = machine_id
        with steps.step('machine-named-from-PB_NAME'):
            need(got_name and got_name.lower() == wanted.lower(), 'the installer was given PB_NAME=%s and set up %s' % (wanted, got_name))
        with steps.step('version'):
            version = json.loads(ok(target.pb('--version', '--json'), 'pb --version').out.strip().splitlines()[-1])
            version = (version.get('data') or version)['version']
            need(not check.pin or version == check.pin, 'the one-liner installed %s, not %s' % (version, check.pin))
            steps.notes.append('%s on %s' % (version, platform))
        with steps.step('rerun-is-a-no-op'):
            result = ok(target.one_liner(recipe['url'], 'PB_RECIPE', recipe['token'], name, check.pin), 'the one-liner again', ALREADY)
            need('Set up ' not in result.text, 'the rerun set the machine up again: ' + result.tail())
        with steps.step('vault-read-with-nobody-online'):
            check.read(target, 'RCP_SECRET', s.marker)
            check.read(peer['device'], 'RCP_SECRET', s.marker)
    with contextlib.suppress(Exception):
        s.unlock()
    with steps.step('tags-at-pairing'):
        need(name in ids and peer.get('name') in ids, 'both machines must be paired')

        def tagged():
            for host in (name, peer['name']):
                rows = check.named(host)
                need(len(rows) == 1 and rows[0]['id'] == ids[host], '%s is listed %d times or with another id' % (host, len(rows)))
                need(rows[0].get('tags') == ['tag:' + tag], '%s has tags %s' % (host, rows[0].get('tags')))
            need(not check.named(s.creator_name)[0].get('tags'), 'the creator got a tag')
        wait_for('both machines tagged', tagged, 60)
    with steps.step('recipe-show-applied'):
        def applied():
            rows = check.shown(tag)['machines']
            need(sorted(m['name'] for m in rows) == sorted([name, peer.get('name', '')]), 'show lists %s' % [m['name'] for m in rows])
            need(all(m['applied_revision'] >= 1 for m in rows), 'not applied yet')
            need(not any((state or {}).get('error') for m in rows for state in (m.get('apply_state') or {}).values()), 'a section failed: %s' % [m.get('apply_state') for m in rows])
        wait_for('both machines applying the recipe', applied, 180, 10)
    names = {}
    with steps.step('reach-allows-exec-both-ways'):
        names['target'], names['peer'] = target.hostname(), peer['device'].hostname()
        check.exec_hostname(target, peer['name'], names['peer'])
        check.exec_hostname(peer['device'], name, names['target'])
    with steps.step('reach-refuses-other-machine'):
        need(peer, 'no peer')
        check.exec_refused(target, s.creator_name, seconds=60)
    with steps.step('reach-refuses-other-action'):
        need(names, 'no reach to compare with')
        # the same peer that just answered exec: a file (action "files") is not in allow
        path = target.write('not-allowed.txt', 'not allowed by the reach\n')
        refused(target.pb('send', path, '--to', peer['name'], timeout=150), 'pb send, an action the reach does not allow', NOT_REACHED)
    with steps.step('reach-update-refuses-then-allows'):
        need(names, 'no reach to compare with')
        s.unlock()
        closed = {k: v for k, v in spec.items() if k != 'reach'}
        said = check.update(tag, closed)
        need('updated to revision 2' in said and 'reach' in said, 'update said: ' + said[-200:])
        check.exec_refused(target, peer['name'])
        said = check.update(tag, spec)
        need('updated to revision 3' in said, 'update said: ' + said[-200:])
        check.exec_hostname(target, peer['name'], names['peer'])
    with steps.step('revoke-removes-the-machines'):
        preview, result = check.confirmed('revoke', tag)
        for host in (name, peer.get('name', '')):
            need(host in preview, 'the revoke preview does not name %s: %s' % (host, preview[-300:]))
        need('2 machines removed' in result.out, 'revoke said: ' + result.tail())
        wait_for('the machines leaving the list', lambda: check.gone(name, peer.get('name', '')), 120)
    with steps.step('tag-has-no-machine-left'):
        policy = ok(creator.pb('policy', 'get'), 'pb policy get').out
        for host in (name, peer.get('name', '')):
            need(not host or 'machine:' + host not in policy, 'the policy still tags %s' % host)
    with steps.step('removed-machine-reads-nothing'):
        check.unreadable(target, s.marker)
    with steps.step('removed-machine-rerun-says-why'):
        check.refuse_install(target, recipe['url'], recipe['token'], name, REMOVED)
    with steps.step('uninstall-removes-the-install'):
        target.uninstall()
        need(not target.present(), 'pb uninstall left behind: %s' % ', '.join(target.present())[:300])
    with steps.step('uninstall-removes-the-shell-hook'):
        left = target.hooks()
        need(not left, 'pb uninstall left the ENV hook line (a pb() shell function for the deleted binary) in %s' % ', '.join(left)[:300])
    if platform in ('macos', 'windows'):
        reverse(check, platform, name, target, recipe['url'])


# ------------------------------------------------------------------ F26-05
APPLY_SOURCE = """// Every section of a recipe in one commented file (PLAN F26).
{
  /* what the paired machine gets */
  "tags": ["%(tag)s"], // a trailing comment
  "reach": [{"dst": ["tag:%(tag)s"], "allow": ["exec"]}],
  "env": {"vaults": ["%(vault)s"]},
  "config": {"repository": "%(repository)s"},
  "preview": [{"port": %(preview)d, "ttl": "2h"}],
  "tunnel": [{"port": %(tunnel)d}],
  "max_machines": 2,
  "expires": "1d", // and trailing commas
}
"""
PREVIEW_PORT, TUNNEL_PORT = 18731, 18732


def items(creator, *args):
    """{id: item} of a pb list --json (all of the throwaway account's)."""
    data = rd.json_data(creator.pb(*args, '--json', '--limit', '200'))
    return {row['id']: row for row in data.get('items') or []}


def inventory(check):
    previews = items(check.s.creator, 'preview', 'list', '--all')
    tunnels = items(check.s.creator, 'tunnel', 'list')
    return previews, tunnels


def check_apply(check, platform):
    steps, s = check.steps, check.s
    creator, tag = s.creator, check.tag
    repository = 'rcp-cfg-' + tag[3:]
    app = 'rcpapp' + secrets.token_hex(6)
    with steps.step('vault-setup'):
        s.vault_ready()
    with steps.step('config-repository-connected'):
        url = f.ENV.get('SMOKE_RECIPE_CONFIG_URL', CONFIG_URL)
        ok(creator.pb('config', 'repository', 'connect', url, '--name', repository, '--json'), 'pb config repository connect')
    name, target = None, None
    with steps.step('target-has-no-paperboat'):
        candidate_name, candidate = target_device(check, platform)
        found = candidate.present() + candidate.hooks()
        need(not found, 'the %s machine already has Paperboat (%s): it is not touched; use a fresh runner' % (platform, ', '.join(found)[:200]))
        name, target = candidate_name, candidate
    if target is None:
        return
    if platform != 'container':
        check.stack.callback(uninstall, steps, target)
    servers = []
    check.stack.callback(lambda: [target.stop_app(pid) for pid in servers])
    with steps.step('local-app-serving'):
        for port in (PREVIEW_PORT, TUNNEL_PORT):
            servers.append(target.start_app(port, app))
    source = APPLY_SOURCE % {'tag': tag, 'vault': s.vault, 'repository': repository, 'preview': PREVIEW_PORT, 'tunnel': TUNNEL_PORT}
    recipe, before = {}, {}
    with steps.step('create-from-commented-file'):
        path = creator.write(tag + '.jsonc', source)
        check.live.append((creator, tag))
        before['previews'], before['tunnels'] = inventory(check)
        result = creator.pb('recipe', 'create', '-f', path, '--json')
        for token in rd.PB_RECIPE_RE.findall(result.text):
            token_parts(token)
        data = rd.json_data(result)
        token_parts(data.get('token', ''))
        key = 'windows' if target.os == 'windows' else 'linux_macos'
        recipe.update(token=data['token'], url=rd.installer_url(data['install'][key], windows=target.os == 'windows'))
        shown = check.shown(tag)['recipe']
        need(shown['source'].replace('\r\n', '\n') == source, 'show does not return the commented file as written')
        parsed = shown['recipe']
        missing = [k for k in ('tags', 'reach', 'env', 'config', 'preview', 'tunnel') if not parsed.get(k)]
        need(not missing, 'the stored recipe lost %s' % ', '.join(missing))
    if not recipe:
        return
    paired = {}
    with steps.step('pair-with-the-one-liner'):
        paired['id'], name = check.install(target, recipe['url'], recipe['token'], name, any_name=True)
    if not paired:
        return
    state = {}

    def row():
        rows = [m for m in check.shown(tag)['machines'] if m['name'] == name]
        need(len(rows) == 1, 'show lists the machine %d times' % len(rows))
        return rows[0]

    def made():
        """(previews, tunnels) the account did not have before the pairing, ended ones included."""
        previews, tunnels = inventory(check)
        return ({k: v for k, v in previews.items() if k not in before['previews']},
                {k: v for k, v in tunnels.items() if k not in before['tunnels']})
    with steps.step('applied-after-pairing'):
        def applied():
            mine = row()
            errors = {k: v.get('error') for k, v in (mine.get('apply_state') or {}).items() if (v or {}).get('error')}
            need(mine['applied_revision'] == 1 and not errors, 'applied revision %s, errors %s; %s' % (mine['applied_revision'], errors, recipe_step(target)))
            _, record = target.apply_record()
            need(record['revision'] == 1 and len(record['previews']) == 1 and len(record['tunnels']) == 1
                 and record['config'] == repository and record['hook'], '%s; the machine has applied %s' % (recipe_step(target), json.dumps(record)[:300]))
            state['record'] = record
        wait_for('every section applied', applied, 420, 10)
    with steps.step('one-preview-one-tunnel'):
        every, tunnels = made()
        # a first attempt that raced the starting host runtime ends its own preview: live ones are what the user has
        previews = {k: v for k, v in every.items() if v.get('state') != 'stopped'}
        need(len(previews) == 1 and len(tunnels) == 1, '%d live previews and %d tunnels were created (the machine recorded %s): %s' % (
            len(previews), len(tunnels), sorted(state['record']['previews'].values()) if 'record' in state else '?',
            json.dumps([{k: row.get(k) for k in ('id', 'state', 'allocation_state', 'created_at')} for row in every.values()])))
        if len(every) > len(previews):
            steps.notes.append('%d ended preview from a failed first attempt' % (len(every) - len(previews)))
        preview, tunnel = next(iter(previews.values())), next(iter(tunnels.values()))
        need(tunnel['name'] == name, 'the tunnel is named %s, not after the machine %s' % (tunnel['name'], name))
        need(preview['owner_machine_id'] == paired['id'], 'the preview belongs to another machine')
        state.update(previews=sorted(every), tunnels=sorted(tunnels), urls={'preview': preview.get('endpoint'), 'tunnel': tunnel.get('stable_endpoint')})

    def serving(what):
        address = state['urls'][what]
        need(address, 'the %s has no address' % what)
        address = address if address.startswith('https://') else 'https://' + address

        def probe():
            done = creator.sh('curl -fsS --max-time 20 %s' % shlex.quote(address + '/'), timeout=60)
            need(done.code == 0 and app in done.out, '%s answers exit %d %s' % (what, done.code, done.tail(200)))
        wait_for('the %s serving the local app' % what, probe, 240, 10)
    for what in ('preview', 'tunnel'):
        with steps.step(what + '-serves-the-app'):
            need('urls' in state, 'nothing was created')
            serving(what)
    with steps.step('config-pull-only'):
        said = ok(target.pb('config', 'sync', 'validate'), 'pb config sync validate').text
        need('pull_only' in said, 'the machine configuration is not pull_only: ' + said[-300:])
        state['config'] = config_assignment(target)
    with steps.step('env-section'):
        check.read(target, 'RCP_SECRET', s.marker)
        if target.os != 'windows':
            state['hooks'] = target.hook_lines()
            need(state['hooks'] and all(n == 1 for n in state['hooks'].values()), 'ENV hook lines: %s' % state['hooks'])
    with steps.step('daemon-restart'):
        state['mtime'], state['at_restart'] = target.apply_record()
        time.sleep(2)
        ok(target.pb('service', 'restart', '--json', timeout=240), 'pb service restart')
        state['restarted'] = True
    with steps.step('apply-runs-again-after-restart'):
        need(state.get('restarted'), 'the daemon was not restarted')

        def ran_again():
            stamp, _ = target.apply_record()
            need(stamp > state['mtime'], 'the apply record has not been rewritten since the restart')
        wait_for('the recipe being applied again by the restarted daemon', ran_again, 300, 10)
    with steps.step('nothing-applied-twice'):
        need(state.get('restarted') and 'previews' in state, 'no state before the restart')
        time.sleep(60)  # past the first retry (15 s) of an apply that failed right after the restart
        _, record = target.apply_record()
        need(record == state['at_restart'], 'the apply record changed: %s -> %s' % (json.dumps(state['at_restart'])[:250], json.dumps(record)[:250]))
        previews, tunnels = made()
        need((sorted(previews), sorted(tunnels)) == (state['previews'], state['tunnels']),
             'after the restart there are %d previews and %d tunnels (new ids: %s)' % (
                 len(previews), len(tunnels), sorted(set(previews) - set(state['previews'])) + sorted(set(tunnels) - set(state['tunnels']))))
        mine = row()
        errors = {k: v.get('error') for k, v in (mine.get('apply_state') or {}).items() if (v or {}).get('error')}
        need(mine['applied_revision'] == 1 and not errors, 'applied revision %s, errors %s' % (mine['applied_revision'], errors))
        if 'config' in state:
            now = config_assignment(target)
            need(now == state['config'], 'the config assignment changed: %s -> %s' % (state['config'], now))
        if 'hooks' in state:
            need(target.hook_lines() == state['hooks'], 'ENV hook lines changed: %s -> %s' % (state['hooks'], target.hook_lines()))
    with steps.step('still-serving-after-restart'):
        need('urls' in state, 'nothing was created')
        for what in ('preview', 'tunnel'):
            serving(what)
    with steps.step('revoke-removes-the-machine'):
        _, result = check.confirmed('revoke', tag)
        need('1 machine removed' in result.out, 'revoke said: ' + result.tail())
        wait_for('the machine leaving the list', lambda: check.gone(name), 120)
    if state.get('urls', {}).get('tunnel'):
        steps.notes.append('config repository %s, preview and tunnel on ports %d/%d' % (repository, PREVIEW_PORT, TUNNEL_PORT))
    if platform != 'container':
        with steps.step('uninstall'):
            target.uninstall()
            need(not target.present(), 'pb uninstall left behind: %s' % ', '.join(target.present())[:300])


def recipe_step(device):
    """pb doctor's line about applying the recipe (why a section has not applied)."""
    said = device.pb('doctor', timeout=120).text
    return next((' '.join(line.split())[:300] for line in said.splitlines() if line.startswith('daemon:recipe:')), 'pb doctor shows no recipe failure')


def config_assignment(device):
    """The parts of this machine's config sync file check that a second apply would change."""
    said = ok(device.pb('config', 'sync', 'validate'), 'pb config sync validate').out
    return [line.strip() for line in said.splitlines() if line.strip().startswith(('Revision:', 'Enabled:', 'Repository:'))]


def uninstall(steps, device):
    try:
        device.uninstall()
        left = device.present()
        need(not left, 'Paperboat is left on the machine: %s' % ', '.join(left)[:200])
        device.drop_hooks()
    except Exception as error:  # noqa: BLE001 recorded as a failure of the check, the other cleanups still run
        steps.failed.append(('uninstall', rd.redact(str(error))[:300]))


def reverse(check, platform, name, target, url):
    """X-47 the other way: this OS is the creator; its recipe and its vault value apply on Linux."""
    steps, s = check.steps, check.s
    made, value = {}, s.marker + platform
    key = 'RCP_FROM_' + platform.upper()
    with steps.step('creator-here:pair'):
        target.drop_hooks()  # judged above; the next install starts on a clean machine
        left = target.present()
        need(not left, 'Paperboat is still on the machine: %s' % ', '.join(left)[:300])
        ok(target.one_liner(url, 'PB_TOKEN', s.enrollment_token(), name + '-c', check.pin), 'the enrollment one-liner')
        made['paired'] = True
    if not made:
        return
    with steps.step('creator-here:write-vault'):
        wait_for('pb env unlock on this machine', lambda: s.unlock(target), 240, 10)
        s.set_value(key, value, target)
    lifecycle(check, target, 'creator-here:')
    recipe = check.tag + 'r'
    with steps.step('creator-here:recipe-installs-on-linux'):
        token, posix_url, _ = check.create(recipe, {'tags': [recipe], 'env': {'vaults': [s.vault]}, 'expires': '1d'}, creator=target)
        host, device = check.bare('from' + platform[:3])
        check.install(device, posix_url, token, host)
        made['host'], made['device'] = host, device
    with steps.step('creator-here:vault-values-read-on-linux'):
        need('device' in made, 'no Linux machine')
        check.read(made['device'], key, value)
        check.read(made['device'], 'RCP_SECRET', s.marker)
    with steps.step('creator-here:revoke'):
        _, result = check.confirmed('revoke', recipe, target)
        need('1 machine removed' in result.out, 'revoke said: ' + result.tail())
        wait_for('the Linux machine leaving the list', lambda: check.gone(made.get('host', '')), 120)


RUNNERS = {'recipe-lifecycle': check_lifecycle, 'recipe-errors': check_errors, 'recipe-vaults': check_vaults,
           'recipe-lifetimes': check_lifetimes, 'recipe-api-tokens': check_api_tokens}


def dispatch(check, check_id):
    if check_id.startswith('recipe-install:'):
        check_install(check, check_id.partition(':')[2])
    elif check_id.startswith('recipe-apply:'):
        check_apply(check, check_id.partition(':')[2])
    else:
        RUNNERS[check_id](check)


def run_check(session, check_id, expect, log, run=dispatch):
    steps = Steps(check_id, log)
    try:
        with contextlib.ExitStack() as stack:
            check = Check(session, steps, stack, expect)
            try:
                run(check, check_id)
            finally:
                check.revoke_all()
    except Exception as error:  # noqa: BLE001 outside a step (a container, the creator): a FAIL with the reason
        steps.failed.append(('harness', rd.redact('%s: %s' % (type(error).__name__, error))))
        steps.say('FAIL harness: %s: %s' % (type(error).__name__, error))
    return steps.result()


def main(argv, checks=CHECKS, run=dispatch, usage=__doc__, receipt='recipes'):
    args, expect = list(argv[1:]), ''
    if '--expect-version' in args:
        at = args.index('--expect-version')
        if at + 1 >= len(args):
            print(usage, file=sys.stderr)
            return 2
        expect = args[at + 1]
        del args[at:at + 2]
    ids = [i for i in args[0].split(',') if i] if len(args) == 1 else []
    if not ids or any(i not in checks for i in ids):
        print(usage, file=sys.stderr)
        return 2
    results, log, setup_failure = {}, [], ''
    try:
        with f.Machines() as machines:
            session = Session(machines)
            for check_id in ids:
                began = time.monotonic()
                passed, detail = run_check(session, check_id, expect, log, run)
                results[check_id] = [passed, detail, round(time.monotonic() - began)]
        leftovers = machines.leftovers
    except Exception as error:  # noqa: BLE001 the account, the creator or a cleanup step
        setup_failure = rd.redact('%s: %s' % (type(error).__name__, error))
        leftovers = []
    kept = f.keep(receipt, '\n'.join(log + [setup_failure] + leftovers) + '\n')
    exit_code = 0
    for check_id in ids:
        passed, detail, seconds = results.get(check_id, [False, 'not run: ' + setup_failure[:250], 0])
        if setup_failure and check_id in results:
            passed, detail = False, 'cleanup failed: %s; %s' % (setup_failure[:150], detail)
        if leftovers:
            passed, detail = False, 'LEFT BEHIND: %s; %s' % ('; '.join(leftovers)[:200], detail)
        if not passed:
            exit_code, detail = 1, '%s log %s' % (detail[:380], kept)
        print(json.dumps({'check': check_id, 'status': 'PASS' if passed else 'FAIL', 'detail': rd.redact(detail)[:600], 'seconds': seconds}), flush=True)
    return exit_code


if __name__ == '__main__':
    sys.exit(main(sys.argv))
