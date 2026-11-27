#!/usr/bin/env python3
"""Every command row of CHECKLIST.md (C-rows) on one signed-in device, driven from this controller over SSH.

  command_sweep.py --ssh HOST --platform L|M|W [--version RELEASE] [--only C058,C059] [--out FILE]

Per row, what "Command rows: what [x] means" asks that can be checked without changing anything:
  help     `pb <command> --help` exits 0 with a Usage line naming the command and no stack trace
  flags    the flags of that help are exactly the flags of cli/docs/cli/pb_<command>.md (read at the tag --version when it exists)
  bad-flag an unknown flag is refused with a short message and no stack trace
  bogus    a command group refuses an unknown subcommand
  json     a read-only use answers valid JSON that reports success (the JSON table below)
  missing  a use that names a machine, session or ID that does not exist is refused with a short message (the MISSING table;
           every entry names something that cannot exist, so nothing is changed)
  signed   signed out (an empty config directory of its own) the account commands say how to sign in
The main use of each command is checked by its family's rows (command_rows.py, tunnel_command_rows.py, config_commands.py,
team_commands.py and the feature rows of the shards); this sweep prints one receipt per row and exits 1 when any failed.
"""
import argparse
import json
import re
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
sys.path.insert(0, str(HERE))
from results_dir import results_dir  # noqa: E402

BAD = re.compile(r'panic:|goroutine \d|Traceback \(most recent|runtime error:|nil pointer')
NOPE = 'pbsweep-no-such-zz'
UNKNOWN_FLAG = '--pbsweep-no-such-flag'

# command (without "pb ") -> the arguments of a read-only use that supports --json
JSON_USES = {c: [] for c in (
    'api-token list', 'auth status', 'config conflict list', 'config path', 'config repository list', 'config show',
    'config status', 'config status-bar show', 'config customize show', 'config customize path', 'config local-access show',
    'config sync path', 'domain list', 'edge list', 'env list', 'env names', 'env host show', 'environments',
    'inbox path', 'inbox requests', 'machine list', 'policy get', 'policy versions', 'port name list',
    'preview list', 'recordings list', 'relay list', 'session list', 'session shared', 'status',
    'team list', 'team invitations', 'tunnel list', 'tunnel policy list', 'update status', 'update settings', 'version',
    'doctor', 'service status', 'send destination')}

# CHECKLIST rows written before a rename, checked under the command's current name (release A: the vault custody commands
# moved from `pb env vault <x>` to `pb env <x>`; `pb env vault` now manages named vaults)
RENAMED = {'env vault ' + word: 'env ' + word for word in ('init', 'lock', 'password', 'recover', 'recovery', 'remove', 'reset', 'resume', 'unlock')}

# a read-only use whose documented answer on a machine without delivered ENV is a refusal that names the way out
NEEDS_VAULT = {'env list', 'env names'}

# command -> arguments that name something that does not exist: refused, nothing changed
MISSING = {
    'exec': [NOPE, '--', 'true'], 'ping': [NOPE], 'wait': [NOPE, '--timeout', '10s'], 'ssh': [NOPE, 'true'],
    'ssh doctor': [NOPE], 'ssh trust-host': [NOPE], 'connect': [NOPE], 'scp': [NOPE + ':/x', 'pbsweep-x'], 'resolve': [NOPE],
    'access machine': [NOPE, '--port', '80'], 'access tunnel': [NOPE],
    'machine rename': [NOPE, 'pbsweep-new-name'], 'machine revoke': [NOPE], 'machine capabilities': [NOPE],
    'machine availability': [NOPE], 'machine tag': [NOPE],
    'session attach': [NOPE], 'session close': [NOPE], 'session delete': [NOPE], 'session join': [NOPE],
    'session participants': [NOPE], 'session rename': [NOPE, 'x'], 'session share': [NOPE], 'session unshare': [NOPE],
    'session remove': [NOPE, NOPE],
    'preview delete': [NOPE], 'preview status': [NOPE], 'preview stop': [NOPE], 'preview inspect': [NOPE],
    'preview replay': [NOPE, NOPE],
    'tunnel show': [NOPE], 'tunnel status': [NOPE], 'tunnel logs': [NOPE], 'tunnel delete': [NOPE], 'tunnel stop': [NOPE],
    'tunnel pause': [NOPE], 'tunnel resume': [NOPE], 'tunnel doctor': [NOPE], 'tunnel rename': [NOPE, 'pbsweep-new'],
    'tunnel inspect': [NOPE], 'tunnel address': [NOPE], 'tunnel extend': [NOPE, '--duration', '1h'],
    'tunnel route list': [NOPE], 'tunnel domain list': [NOPE], 'tunnel policy revoke': [NOPE],
    'inbox approve': [NOPE], 'inbox decline': [NOPE], 'send cancel': [NOPE], 'send status': [NOPE],
    'team get': [NOPE], 'team delete': [NOPE], 'team accept': [NOPE], 'team decline': [NOPE], 'team leave': [NOPE],
    'team invitation': [NOPE], 'team bucket get': [NOPE], 'domain show': [NOPE + '.example.test'],
    'domain verify': [NOPE + '.example.test'], 'domain remove': [NOPE + '.example.test'],
    'recordings play': [NOPE], 'recordings export': [NOPE], 'api-token revoke': [NOPE],
    'config conflict show': [NOPE], 'config conflict compare': [NOPE], 'config repository credentials remove': [NOPE],
    'switch': [NOPE], 'auth switch': [NOPE], 'port name unset': [NOPE],
}

# commands whose signed-out use must say how to sign in
SIGNED_OUT = ('machine list', 'environments', 'session list', 'preview list', 'tunnel list', 'team list', 'domain list', 'api-token list')

# runs on the device: a list of {key, args, signed_out} on stdin, one JSON result per line on stdout
AGENT = r'''
import glob, json, os, shutil, subprocess, sys, tempfile
def pb():
    if os.name == 'nt':
        found = sorted(glob.glob(r'C:\Program Files\Paperboat\users\*\bin\pb.exe'))
        return found[0] if found else 'pb.exe'
    return os.path.expanduser('~/.local/bin/pb')
exe = pb()
for job in json.loads(sys.argv_jobs):
    env = dict(os.environ)
    if os.name != 'nt':
        env.setdefault('XDG_RUNTIME_DIR', '/run/user/%d' % os.getuid())
    home = None
    if job.get('signed_out'):
        # Windows: a directory under the profile with its inherited ACL; Python's private temp directory ACL stops pb itself
        home = os.path.join(os.path.expanduser('~'), 'pbsweep-out-%d-%s' % (os.getpid(), job['key'].split(':')[0])) if os.name == 'nt' else tempfile.mkdtemp(prefix='pbsweep-out-')
        os.makedirs(home, exist_ok=True)
        env.update(HOME=home, USERPROFILE=home)
        if os.name == 'nt':  # the layout of a real profile
            env.update(APPDATA=os.path.join(home, 'AppData', 'Roaming'), LOCALAPPDATA=os.path.join(home, 'AppData', 'Local'))
            made = [env['APPDATA'], env['LOCALAPPDATA']]
        else:
            env.update(XDG_CONFIG_HOME=os.path.join(home, 'config'), XDG_DATA_HOME=os.path.join(home, 'data'), XDG_STATE_HOME=os.path.join(home, 'state'))
            made = [env['XDG_CONFIG_HOME'], env['XDG_DATA_HOME'], env['XDG_STATE_HOME']]
        for path in made:
            os.makedirs(path, exist_ok=True)
    try:
        done = subprocess.run([exe] + job['args'], stdin=subprocess.DEVNULL, capture_output=True, timeout=job.get('timeout', 60), env=env, cwd=home)
        result = {'key': job['key'], 'rc': done.returncode, 'out': done.stdout.decode('utf-8', 'replace')[:4000000], 'err': done.stderr.decode('utf-8', 'replace')[-8000:]}
    except subprocess.TimeoutExpired:
        result = {'key': job['key'], 'rc': -1, 'out': '', 'err': 'pbsweep: timed out'}
    if home:
        shutil.rmtree(home, ignore_errors=True)
    print('PBSWEEP ' + json.dumps(result), flush=True)
'''


def checklist_commands():
    rows = {}
    for line in (ROOT / 'CHECKLIST.md').read_text().splitlines():
        found = re.match(r'\|\s*(C\d{3})\s*\|\s*`pb([^`]*)`', line)
        if found:
            words = [w for w in found.group(2).split() if not w.startswith(('<', '[', '-'))]
            command = ' '.join(words)
            rows[found.group(1)] = RENAMED.get(command, command)
    return rows


def doc_text(command, version):
    name = 'cli/docs/cli/%s.md' % '_'.join(['pb'] + command.split())
    if version:
        tagged = subprocess.run(['git', '-C', str(ROOT), 'show', '%s:%s' % (version, name)], capture_output=True, text=True)
        if tagged.returncode == 0:
            return tagged.stdout
        if subprocess.run(['git', '-C', str(ROOT), 'rev-parse', '-q', '--verify', version + '^{commit}'], capture_output=True).returncode == 0:
            return None
    path = ROOT / name
    return path.read_text() if path.is_file() else None


def flags_of_doc(text):
    block = re.search(r'^### Options\n\n```\n(.*?)^```', text, re.S | re.M)
    return set(re.findall(r'^\s+(?:-\w, )?--([\w-]+)', block.group(1), re.M)) if block else set()


def flags_of_help(text):
    block = re.search(r'^Flags:\n(.*?)(?:\n\n|^Global Flags:|\Z)', text, re.S | re.M)
    return set(re.findall(r'^\s+(?:-\w, )?--([\w-]+)', block.group(1), re.M)) if block else set()


def run_jobs(host, platform, jobs):
    """All jobs in one SSH session on the device; the agent and its jobs go over stdin, nothing is left on the device."""
    program = 'import sys\nsys.argv_jobs = %r\n%s' % (json.dumps(jobs), AGENT)
    python = 'python -X utf8 -' if platform == 'W' else 'python3 -'
    done = subprocess.run(['ssh', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=20', '-o', 'ServerAliveInterval=30', host, python],
                          input=program.encode(), capture_output=True, timeout=2400)
    results = {}
    for line in done.stdout.decode('utf-8', 'replace').splitlines():
        if line.startswith('PBSWEEP '):
            value = json.loads(line[8:])
            results[value['key']] = value
    if len(results) != len(jobs):
        raise SystemExit('the device answered %d of %d commands: %s' % (len(results), len(jobs), done.stderr.decode('utf-8', 'replace')[-800:]))
    return results


def short_refusal(result):
    text = result['out'] + result['err']
    if result['rc'] == 0:
        return 'was accepted (exit 0)'
    if result['rc'] == -1:
        return 'did not answer within the time limit'
    if BAD.search(text):
        return 'printed a stack trace'
    if not text.strip():
        return 'failed without any message'
    return None


def tail(result):
    return ' '.join((result['err'].strip() or result['out'].strip()).split())[:300]


def json_value(text):
    try:
        return json.loads(text)
    except ValueError:
        lines = [line for line in text.splitlines() if line.strip()]
        return [json.loads(line) for line in lines] if lines else None


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--ssh', required=True)
    parser.add_argument('--platform', required=True, choices=['L', 'M', 'W'])
    parser.add_argument('--version', default='')
    parser.add_argument('--only', default='')
    parser.add_argument('--machine', default='', help="another machine of the account (send list --on)")
    parser.add_argument('--out')
    args = parser.parse_args()
    rows = rows_all = checklist_commands()
    if args.only:
        rows = {k: v for k, v in rows.items() if k in args.only.split(',')}
    if not rows:
        raise SystemExit('no command rows selected')
    jobs = []
    for ident, command in rows.items():
        words = command.split()
        jobs.append({'key': ident + ':help', 'args': words + ['--help']})
        if command:
            jobs.append({'key': ident + ':badflag', 'args': words + [UNKNOWN_FLAG]})
        # a group with no main use of its own: an unknown word is not one of its commands (bare `pb` and `pb status`-like
        # groups that take an argument are excluded below by their help having no "Available Commands")
        if command and any(other.startswith(command + ' ') for other in rows_all.values()):
            jobs.append({'key': ident + ':bogus', 'args': words + [NOPE]})
        if command in JSON_USES:
            jobs.append({'key': ident + ':json', 'args': words + JSON_USES[command] + ['--json'], 'timeout': 120})
        if command == 'policy schema':  # the schema itself is the JSON; the command has no --json
            jobs.append({'key': ident + ':document', 'args': words})
        if command == 'send list' and args.machine:
            jobs.append({'key': ident + ':json', 'args': words + ['--on', args.machine, '--json'], 'timeout': 120})
        if command in MISSING:
            jobs.append({'key': ident + ':missing', 'args': words + MISSING[command], 'timeout': 90})
        if command in SIGNED_OUT:
            jobs.append({'key': ident + ':signed', 'args': words, 'signed_out': True})
    results = run_jobs(args.ssh, args.platform, jobs)
    report = []
    for ident, command in sorted(rows.items()):
        failures = []
        name = ('pb ' + command).strip()
        shown = results[ident + ':help']
        usage = shown['out'] + shown['err']
        if shown['rc'] != 0 or 'Usage:' not in usage or name not in usage:
            failures.append('help: exit %d, or no Usage line naming "%s": %s' % (shown['rc'], name, tail(shown)))
        if BAD.search(usage):
            failures.append('help: stack trace')
        group = 'Available Commands:' in usage
        doc = doc_text(command, args.version)
        if doc is None:
            failures.append('flags: cli/docs/cli has no page for "%s"' % name)
        elif shown['rc'] == 0:
            documented, offered = flags_of_doc(doc) - {'help'}, flags_of_help(usage) - {'help'}
            if documented != offered:
                failures.append('flags: only in the docs %s; only in --help %s' % (sorted(documented - offered), sorted(offered - documented)))
        for suffix, label in (('badflag', 'unknown flag'), ('missing', 'missing target %s' % MISSING.get(command))):
            result = results.get('%s:%s' % (ident, suffix))
            if result:
                problem = short_refusal(result)
                if problem is None and len(result['out'] + result['err']) > 12000:
                    problem = 'answered with %d bytes' % len(result['out'] + result['err'])
                if problem:
                    failures.append('%s %s: %s' % (label, problem, tail(result)))
        result = results.get(ident + ':bogus')
        if result and group and '[' not in next((line for line in usage.split('Usage:', 1)[1].splitlines() if line.strip()), '').replace('[command]', '').replace('[flags]', ''):
            if short_refusal(result):
                failures.append('unknown subcommand %s: %s' % (short_refusal(result), tail(result)))
        result = results.get(ident + ':json')
        if result:
            try:
                value = json_value(result['out'])
            except ValueError:
                value = None
            if value is None:
                failures.append('json: output is not JSON (exit %d): %s' % (result['rc'], tail(result)))
            elif command in NEEDS_VAULT and result['rc'] != 0 and 'pb env unlock' in result['out']:
                pass
            elif result['rc'] != 0 or (isinstance(value, dict) and value.get('ok') is False):
                failures.append('json: reported failure (exit %d): %s' % (result['rc'], ' '.join(result['out'].split())[:300]))
        result = results.get(ident + ':document')
        if result:
            try:
                json.loads(result['out'])
            except ValueError:
                failures.append('schema output is not a JSON document (exit %d): %s' % (result['rc'], tail(result)))
        result = results.get(ident + ':signed')
        if result:
            text = result['out'] + result['err']
            if result['rc'] == 0 or not re.search(r'pb (auth )?login|sign(ed)?[ -]?in|signed out|pb pair', text, re.I) or BAD.search(text):
                failures.append('signed out: no clear way to sign in (exit %d): %s' % (result['rc'], tail(result)))
        checks = sorted(k.split(':')[1] for k in results if k.startswith(ident + ':'))
        receipt = {'check': 'command-sweep', 'row': ident, 'command': name, 'platform': args.platform, 'group': group,
                   'checks': checks + ['flags'], 'status': 'FAIL' if failures else 'PASS', 'failures': failures}
        report.append(receipt)
        print(json.dumps(receipt), flush=True)
    out = Path(args.out) if args.out else results_dir(__file__) / ('command-sweep-%s.json' % args.platform)
    out.write_text(json.dumps(report, indent=1))
    failed = [r['row'] for r in report if r['status'] == 'FAIL']
    print('%s command-sweep %s: %d rows, %d failed%s -> %s' % ('FAIL' if failed else 'PASS', args.platform, len(report), len(failed),
                                                          ' (%s)' % ','.join(failed) if failed else '', out))
    sys.exit(1 if failed else 0)


if __name__ == '__main__':
    main()
