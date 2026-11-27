#!/usr/bin/env python3
"""Rows that need an account of their own to break: CHECKLIST F10-07 and F1-03. recipe_checks.py's harness: one throwaway console
account and paired creator container per run (so the smoke account's ENV password never leaves the controller), apart from the
recipe rows' account because a vault reset replaces the account's ENV keys.

usage: account_rows.py ID[,ID...] [--expect-version V]
  env-vault-reset-locked:container  F10-07 `pb env reset` on a LOCKED vault (the lost password and recovery code path): without the
  env-vault-reset-locked:macos      exact confirmation nothing changes; with it the personal values are gone, the key epoch moves,
  env-vault-reset-locked:windows    the new password unlocks and the old one does not. container is headless Linux (keys in memory).
  held-web-ports:container          F27-05 a web server holds 0.0.0.0:80 and :443 before Paperboat starts (Traefik, Caddy). Linux
  held-web-ports:linux              (container: a systemd machine) and macOS: the guard moves its gateway to free ports behind a
  held-web-ports:macos              redirect (Linux nft, macOS PF), pb doctor stays healthy and the machine opens its own address;
  held-web-ports:windows            after the web server stops it is still healthy and serving, and pb uninstall leaves no redirect.
                                    Windows (no redirect): IIS holds :80, pb doctor is not healthy and names the port, and is healthy
                                    again by itself after IIS stops.
  pair-token-refusals:linux         F1-03 on a clean machine: the other OS's pasted command stops before anything is installed; a used,
  pair-token-refusals:macos         a revoked and an expired token (the real 10 minute lifetime) are each refused by name by the
  pair-token-refusals:windows       installer one-liner, no machine is added, and a fresh token then pairs (nothing half-done is left).
                                    The wrong-platform paste needs PowerShell on Linux and macOS and Git bash on Windows.
The real machines are the throwaway ones of the recipe rows (SMOKE_RECIPE_<OS>_SSH); each is left without Paperboat. Secrets travel
on stdin only. Receipts as recipe_checks.py.
"""
import datetime
import re
import secrets
import sys
import time

import fixture_checks as f
import https_front_desk
import recipe_checks as rc
import recipe_devices as rd

VAULT_PLATFORMS = ('container', 'macos', 'windows')
TOKEN_PLATFORMS = ('linux', 'macos', 'windows')
PORT_PLATFORMS = ('container', 'linux', 'macos', 'windows')
CHECKS = ('env-vault-reset-locked:container',
          'held-web-ports:container',
          'env-vault-reset-locked:windows',
          'env-vault-reset-locked:macos',)
BUSY = 'another program is using port 80'  # in both of pb doctor's busy-port titles
DARWIN_ANCHOR = 'com.apple/000.paperboat-machineguard'
GUARD = 'paperboat-machineguard.service'
# the two lines `pb machine add` prints (one token for both)
POSIX_ADD = re.compile(r"curl -fsSL (https://[A-Za-z0-9._:/-]+) \| PB_TOKEN='([A-Za-z0-9_.-]+)' sh")
WINDOWS_ADD = re.compile(r"\$env:PB_TOKEN='([A-Za-z0-9_.-]+)'; irm (https://[A-Za-z0-9._:/-]+) \| iex")
REFUSALS = {'used': 'The enrollment token was already used.', 'revoked': 'The enrollment token was revoked or invalid.',
            'expired': 'The enrollment token expired.'}
POSIX_WRONG = 'pb installer: this command is for Windows.'  # install.ps1 under pwsh on Linux or macOS
WINDOWS_WRONG = 'pb installer: only macOS and Linux are supported'  # install.sh in Git bash on Windows
GIT_BASH_PASTE = r'''
$bash = 'C:\Program Files\Git\bin\bash.exe'
if (-not (Test-Path -LiteralPath $bash)) { 'NO GIT BASH'; exit 127 }
$env:PB_TOKEN = [string]$req.value
$ErrorActionPreference = 'Continue'
& $bash -c ('set -o pipefail; curl -fsSL ' + $req.url + ' | sh') 2>&1 | ForEach-Object { "$_" }
exit $LASTEXITCODE
'''

need, ok, refused, wait_for = rc.need, rc.ok, rc.refused, rc.wait_for


def installers(creator):
    """(Linux and macOS installer URL, Windows installer URL) as `pb machine add` prints them."""
    data = rd.json_data(creator.pb('machine', 'add', '--json'))
    posix = POSIX_ADD.fullmatch(data.get('linux_macos_command', '').strip())
    windows = WINDOWS_ADD.fullmatch(data.get('windows_command', '').strip())
    if posix:
        rd.SECRETS.add(posix.group(2))
    if windows:
        rd.SECRETS.add(windows.group(1))
    need(posix and windows, 'pb machine add did not print the two install commands: %s' % rd.redact(str(data))[:300])
    return posix.group(1), windows.group(2)


def mint(session, revoke=False):
    """A one-use enrollment token of the throwaway account; revoke cancels it as closing the console's Add machine dialog does."""
    if not revoke:
        return session.enrollment_token()
    env = dict(session.m.env, CANCEL='1')
    token = f.run(['node', str(f.HERE / 'mint_enrollment_token.mjs')], env=dict(env, STATE=str(session.state)), timeout=180).stdout.strip()
    need(token, 'no revoked enrollment token')
    rd.SECRETS.add(token)
    return token


def real_target(check, platform):
    """(name, clean real machine), uninstalled again when the check ends."""
    name, target = rc.target_device(check, platform)
    found = target.present() + target.hooks()
    need(not found, 'the %s machine already has Paperboat (%s): it is not touched; use a fresh runner' % (platform, ', '.join(found)[:200]))
    if platform != 'container':
        check.stack.callback(rc.uninstall, check.steps, target)
    return name, target


# ------------------------------------------------------------------ F10-07
def check_vault_reset(check, platform):
    steps, s = check.steps, check.s
    device, state = None, {}
    old, new = s.password, secrets.token_hex(12) + 'Bb2!'
    rd.SECRETS.add(new)
    key, value = 'PB_RESET_MARK', 'resetmark' + secrets.token_hex(6)

    def unlock(password):
        return device.pb('env', 'unlock', '--password-stdin', stdin=password)

    def lock():
        ok(device.pb('env', 'lock'), 'pb env lock')

    def inventory():
        data = rd.json_data(device.pb('env', 'list', '--json'))
        return sorted(data.get('names') or []), data['key_epoch']

    def locked():
        refused(device.pb('env', 'set', 'PB_WHILE_LOCKED', '--value-stdin', stdin='x'), 'setting a value while locked', 'locked')

    with steps.step('vault-with-a-value'):
        if not getattr(s, 'personal_vault', False):
            wait_for('pb env init on the creator', lambda: ok(s.creator.pb('env', 'init', '--password-stdin', stdin=s.password), 'pb env init'), 240, 10)
            s.personal_vault = True
        if platform == 'container':
            device = s.creator
            ok(unlock(old), 'pb env unlock')
        else:
            _, device = enrolled(check, platform)
            wait_for('pb env unlock on this machine', lambda: ok(unlock(old), 'pb env unlock'), 240, 10)
        ok(device.pb('env', 'set', key, '--value-stdin', stdin=value), 'pb env set')
        state['names'], state['epoch'] = inventory()
        need(key in state['names'], 'the value is not listed: %s' % state['names'])
        account = rd.json_data(device.pb('auth', 'status', '--json'))['account']['id']
        state['confirm'] = 'RESET ENV ' + account
        state['recovery'] = device.write('reset-input.txt', 'x') + '.' + secrets.token_hex(4) + '.recovery'
    if 'recovery' not in state:
        return

    def reset(*confirm):
        return device.pb('env', 'reset', *confirm, '--password-stdin', '--recovery-file', state['recovery'], stdin=new)
    with steps.step('locked'):
        lock()
        locked()
        state['locked'] = True
    with steps.step('without-the-exact-confirmation-nothing-changes'):
        need(state.get('locked'), 'the vault was not locked')
        wrong = {'no --confirm': (), 'another account': ('--confirm', 'RESET ENV account_wrong'),
                 'lower case': ('--confirm', state['confirm'].replace('RESET ENV', 'reset env')), 'the words alone': ('--confirm', 'RESET ENV')}
        for what, confirm in wrong.items():
            done = refused(reset(*confirm), 'reset with ' + what, '--confirm must exactly match')
            need(new not in done.text, 'the refusal shows the password')
        locked()  # still locked
        need(unlock(new).code != 0, 'the new password of a refused reset unlocks')
        ok(unlock(old), 'pb env unlock with the old password after the refused resets')
        need(inventory() == (state['names'], state['epoch']), 'the refused resets changed the vault: %s' % (inventory(),))
        lock()
    with steps.step('reset-while-locked'):
        locked()
        ok(reset('--confirm', state['confirm']), 'pb env reset on the locked vault')
        state['reset'] = True
        s.password = new
    with steps.step('personal-values-deleted-new-keys'):
        need(state.get('reset'), 'the vault was not reset')
        names, epoch = inventory()
        need(not names and epoch > state['epoch'], 'after the reset: names %s, key epoch %s -> %s' % (names, state['epoch'], epoch))
        steps.notes.append('key epoch %s -> %s on %s' % (state['epoch'], epoch, platform))
    with steps.step('old-password-refused-new-unlocks'):
        need(state.get('reset'), 'the vault was not reset')
        lock()
        refused(unlock(old), 'unlock with the old password', [])
        locked()
        ok(unlock(new), 'pb env unlock with the new password')
        ok(device.pb('env', 'set', key, '--value-stdin', stdin=value + 'two'), 'pb env set after the reset')
        need(key in inventory()[0], 'a value set after the reset is not listed')


# ------------------------------------------------------------------ F1-03
def paste_other_platform(target, posix_url, windows_url, token):
    """The command Add machine prints for the OTHER platform, pasted into the shell it would be pasted into here."""
    if target.os == 'windows':
        code, out, err = target.device.snippet('the Linux command in Git bash', GIT_BASH_PASTE, 600, url=posix_url, value=token)
        return rd.Result(code, out.decode(errors='replace'), err.decode(errors='replace')), WINDOWS_WRONG
    script = 'IFS= read -r v; export PATH="$PATH:/opt/homebrew/bin:/usr/local/bin"; command -v pwsh >/dev/null || { echo "NO POWERSHELL"; exit 127; }; PB_TOKEN="$v" pwsh -NoProfile -NonInteractive -Command %s 2>&1' % (
        rd.shlex.quote("$ErrorActionPreference = 'Stop'; irm %s | iex" % windows_url))
    return target.sh(script, stdin=token + '\n', timeout=600), POSIX_WRONG


def check_token_refusals(check, platform):
    steps, s = check.steps, check.s
    name, target, tokens, urls = None, None, {}, {}
    with steps.step('clean-machine-and-tokens'):
        name, target = real_target(check, platform)
        posix_url, windows_url = installers(s.creator)
        urls.update(posix=posix_url, windows=windows_url, own=windows_url if target.os == 'windows' else posix_url)
        tokens['expired'] = mint(s)
        tokens['expires'] = time.monotonic() + 10 * 60 + 45  # the server's pairing lifetime, plus slack
        tokens['at'] = datetime.datetime.now(datetime.timezone.utc).strftime('%H:%M:%SZ')
    if 'expired' not in tokens:
        return
    with steps.step('other-platform-command'):
        done, message = paste_other_platform(target, urls['posix'], urls['windows'], mint(s))
        need(done.code != 127, 'no shell to paste the other command into on this machine: ' + done.tail())
        refused(done, "the other platform's command", message)
        left = target.present() + target.hooks()
        need(not left, "the other platform's command left %s" % ', '.join(left)[:300])

    def not_added():
        if target.present():  # the installer installs pb before it pairs
            status = target.pb('auth', 'status', '--json')
            need('"signed_in":true' not in status.text.replace(' ', ''), 'the refused machine is signed in: ' + status.tail())
            need(target.pb('machine', 'list').code != 0, 'the refused machine can list the account')
        check.gone(name)

    def refuse(kind):
        done = refused(target.one_liner(urls['own'], 'PB_TOKEN', tokens[kind], name, check.pin), 'the enrollment one-liner with a %s token' % kind, REFUSALS[kind])
        need('create a new enrollment with add machine' in ' '.join(done.text.split()).lower(), 'no way out in the refusal: ' + done.tail())
        need(tokens[kind] not in done.text, 'the refusal prints the token')
        not_added()
    with steps.step('used-token'):
        tokens['used'] = mint(s)
        spare_name, spare = check.bare('spare')
        ok(spare.one_liner(urls['posix'], 'PB_TOKEN', tokens['used'], spare_name, check.pin), 'the enrollment one-liner on the other machine')
        wait_for('the other machine in the list', lambda: need(check.named(spare_name), 'not listed'), 120)
        refuse('used')
        need(len(check.named(spare_name)) == 1, 'the machine that used the token is gone')
    with steps.step('revoked-token'):
        tokens['revoked'] = mint(s, revoke=True)
        refuse('revoked')
    with steps.step('expired-token'):
        time.sleep(max(0, tokens['expires'] - time.monotonic()))
        steps.say('expired token minted at %s' % tokens['at'])
        refuse('expired')
    with steps.step('fresh-token-pairs-after-the-refusals'):
        ok(target.one_liner(urls['own'], 'PB_TOKEN', mint(s), name, check.pin), 'the enrollment one-liner with a fresh token')
        wait_for('the machine online', lambda: need([m for m in check.named(name) if m.get('online')], 'not online'), 180)
        need(len(check.named(name)) == 1, 'the machine is listed %d times' % len(check.named(name)))
    steps.notes.append(platform)


# ------------------------------------------------------------------ F27-05
def enrolled(check, platform):
    """(name, device) of a machine of the throwaway account: a bare container or the clean real machine, installed and paired by
    the printed enrollment command (a check may uninstall it: nothing else has to revoke it)."""
    name, target = real_target(check, platform)
    posix_url, windows_url = installers(check.s.creator)
    ok(target.one_liner(windows_url if target.os == 'windows' else posix_url, 'PB_TOKEN', mint(check.s), name, check.pin), 'the enrollment one-liner')
    return name, target


def doctor_text(device):
    done = device.pb('doctor', timeout=120)
    return ' '.join(done.text.split())


def wait_doctor(device, what, healthy, contains='', seconds=300):
    def probe():
        said = doctor_text(device)
        need(('doctor: healthy' in said) == healthy and contains in said, 'pb doctor says: ' + rd.redact(said)[:400])
    wait_for(what, probe, seconds, 10)


def root(device, script, timeout=90):
    done = device.sh('sudo -n sh -c %s' % rd.shlex.quote(script), timeout=timeout)
    ok(done, script.split()[0] + ' as root')
    return done.out.strip()


class WebServer:
    """The web server that holds the machine's ports: only the processes it started are ever stopped."""

    def __init__(self, device):
        self.device, self.pids, self.iis = device, [], None  # iis: the startup type it had before we started it

    def start(self, marker, secure, plain, bind='0.0.0.0'):
        """Linux and macOS: TLS on bind:secure and a redirect on bind:plain (0: any free port). Returns the two ports."""
        d = self.device
        script = d.write('holder.py', https_front_desk.HOLDER.replace("b'tls443'", 'b%r' % marker).replace("'0.0.0.0'", repr(bind)))
        work = script.rsplit('/', 1)[0]
        ok(d.sh('cd %s && openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=held -keyout k -out c >/dev/null 2>&1' % rd.shlex.quote(work)), 'openssl req')
        log = '%s/holder-%d.log' % (work, len(self.pids))
        line = 'cd %s && { nohup python3 holder.py c k %d %d >%s 2>&1 </dev/null & echo $!; }' % (rd.shlex.quote(work), secure, plain, rd.shlex.quote(log))
        pid = root(d, line) if 0 < secure < 1024 else ok(d.sh(line), 'start the web server').out.strip()
        need(pid.isdigit(), 'no web server PID: %s' % pid[-200:])
        self.pids.append(int(pid))
        found = {}

        def ready():
            said = d.sh('cat %s' % rd.shlex.quote(log), timeout=30).out.split()
            need(said[:1] == ['ready'], 'the web server could not take ports %d and %d: %s' % (secure, plain, ' '.join(said)[-200:]))
            found['ports'] = int(said[1]), int(said[2])
        wait_for('the web server', ready, 30, 2)
        return found['ports']

    def start_iis(self):
        """Windows: IIS takes :80 through HTTP.sys. Only a stopped IIS is started (a disabled one is enabled for the check); stop()
        stops that one and gives it its startup type back."""
        _, said = self.device.device.ps('look at IIS', "$s = Get-Service W3SVC -ErrorAction Stop; '{0} {1}' -f $s.Status, $s.StartType", timeout=90)
        status, startup = said.split()[-2:]
        need(status == 'Stopped' and startup in ('Automatic', 'Manual', 'Disabled'), 'IIS is not a stopped service here: %s %s' % (status, startup))
        self.iis = startup
        self.device.device.ps('start IIS', "if ($req.startup -eq 'Disabled') { Set-Service W3SVC -StartupType Manual }; Start-Service W3SVC", timeout=180, startup=startup)

    def stop(self):
        if self.iis:
            self.device.device.ps('stop IIS', 'Stop-Service W3SVC -Force -ErrorAction SilentlyContinue; Set-Service W3SVC -StartupType $req.startup',
                                  timeout=180, startup=self.iis)
            self.iis = None
        while self.pids:
            self.device.sh('sudo -n kill %d 2>/dev/null; true' % self.pids.pop(), timeout=60)


def listening(device, port):
    """Who listens on the port, with the PIDs: ss on Linux, lsof on macOS."""
    command = 'sudo -n ss -H -ltnp "sport = :%d"' if device.os == 'linux' else 'sudo -n lsof -nP -Fp -iTCP:%d -sTCP:LISTEN'
    return device.sh(command % port, timeout=60).out


def check_held_ports(check, platform):
    steps = check.steps
    name, device = None, None
    with steps.step('machine'):
        name, device = enrolled(check, platform)
    if device is None:
        return
    linux, windows = device.os == 'linux', device.os == 'windows'
    web = WebServer(device)
    check.stack.callback(web.stop)  # registered after the uninstall callback: the ports are free again before it
    marker = 'held' + secrets.token_hex(5)
    state = {}
    with steps.step('healthy-before'):
        wait_doctor(device, 'pb doctor healthy before the web server', True, seconds=300)
    with steps.step('web-server-holds-80-443-first'):
        ok(device.pb('service', 'stop', '--json', timeout=180), 'pb service stop')
        try:
            if linux:
                root(device, 'systemctl stop ' + GUARD)  # the web server was there first
            if windows:
                web.start_iis()
            else:
                web.start(marker, 443, 80)
            state['held'] = True
        finally:
            if linux:
                root(device, 'systemctl start ' + GUARD)
            ok(device.pb('service', 'start', '--json', timeout=180), 'pb service start')
    if not windows:
        with steps.step('own-address-opens-while-held'):
            need(state.get('held'), 'the ports are not held')
            domain = rd.json_data(device.pb('config', 'local-access', 'show', '--json'))['domain']
            state['url'] = 'https://zz%s.%s.%s/who' % (secrets.token_hex(3), name, domain)
            ok(device.pb('port', 'front-desk', name, '443', 'https'), 'pb port front-desk')

            def opens():
                done = device.sh("curl -fsS --noproxy '*' --max-time 15 %s" % rd.shlex.quote(state['url']), timeout=60)
                need(done.code == 0 and done.out.strip() == marker, 'its own address: exit %d %s' % (done.code, done.tail(200)))
            state['opens'] = opens
            wait_for('the machine opening its own address with 80/443 held', opens, 240, 10)
        with steps.step('gateway-redirected'):
            need(state.get('held'), 'the ports are not held')
            if linux:
                rules = root(device, 'nft list ruleset')
                redirects = re.findall(r'tcp dport (443|80) dnat ip to [0-9.]+:(\d+)', rules)
            else:
                rules = root(device, "pfctl -a '%s' -s nat 2>/dev/null" % DARWIN_ANCHOR)
                redirects = [(port.replace('https', '443').replace('http', '80'), to)
                             for port, to in re.findall(r'port = (443|80|https|http) -> \S+ port (\d+)', rules)]
            need({port for port, _ in redirects} == {'443', '80'}, 'no redirect for both held ports: %s in %s' % (redirects, rules[-300:]))
            for port in (443, 80):
                said = listening(device, port)
                need(re.search(r'(?:pid=|^p)%d\b' % web.pids[-1], said, re.M), 'the web server (PID %d) does not hold port %d: %s' % (web.pids[-1], port, said[-200:]))
        with steps.step('doctor-healthy-while-held'):
            time.sleep(20)  # past a reconcile
            wait_doctor(device, 'pb doctor healthy with the gateway redirected', True, seconds=120)
    else:
        with steps.step('doctor-names-the-held-port'):
            need(state.get('held'), 'the ports are not held')
            wait_doctor(device, 'pb doctor not healthy because of the held port', False, BUSY, seconds=240)
            said = doctor_text(device)
            need('back on by itself' in said, 'doctor gives no recovery: ' + rd.redact(said)[:400])
            steps.notes.append(re.search(r'[^.]*%s[^.]*' % BUSY, said).group(0)[-200:])
        with steps.step('machine-still-works-while-degraded'):
            check.exec_hostname(check.s.creator, name, device.hostname(), seconds=180)
    with steps.step('recovers-after-the-web-server-stops'):
        need(state.get('held'), 'the ports were never held')
        web.stop()
        wait_doctor(device, 'pb doctor healthy after the web server stopped', True, seconds=420)
    if not windows:
        with steps.step('own-address-after-the-web-server-stops'):
            need('opens' in state, 'its own address was never set up')
            web.start(marker, 443, 80, bind='127.0.0.1')  # the application back on 443, no longer on every address
            wait_for('the machine opening its own address after the web server stopped', state['opens'], 240, 10)
            web.stop()
        with steps.step('uninstall-leaves-no-redirect'):
            list_rules = 'nft list tables' if linux else "pfctl -a '%s' -s nat 2>/dev/null; true" % DARWIN_ANCHOR
            need(('paperboat' in root(device, list_rules)) if linux else 'port' in root(device, list_rules), 'there is no Paperboat redirect to remove')
            device.uninstall()
            left = root(device, 'nft list ruleset' if linux else list_rules)
            need(('paperboat' not in left.lower()) if linux else 'port' not in left, 'pb uninstall left the redirect: %s' % left[-300:])
    steps.notes.append(platform)


def dispatch(check, check_id):
    kind, _, platform = check_id.partition(':')
    {'env-vault-reset-locked': check_vault_reset, 'pair-token-refusals': check_token_refusals,
     'held-web-ports': check_held_ports}[kind](check, platform)


if __name__ == '__main__':
    sys.exit(rc.main(sys.argv, checks=CHECKS, run=dispatch, usage=__doc__, receipt='account-rows'))
