#!/usr/bin/env python3
"""F12-04 / F12-05 / F12-06 with two real accounts, run from the control host.

OWNER is an enrolled machine of the team owner account (commands run over ssh); MEMBER is an enrolled machine of a
second account (commands run through MEMBER_CMD, e.g. `sudo docker exec -u pb lane-b-pujan`). The team must exist, be
paid, and have both accounts as active members. Everything this check creates it removes again.

Usage: team_two_account.py --team TEAM --owner-ssh pbvm1 --machine MACHINE_ID \
         --member-cmd 'sudo docker exec -u pb -e HOME=/home/pb -e XDG_RUNTIME_DIR=/run/user/1001 lane-b-pujan' \
         --member-machine MEMBER_MACHINE_ID --suspend-cmd 'ssh root@coolify ...'
"""
import argparse
import json
import shlex
import subprocess
import sys
import time

PB = '~/.local/bin/pb'


class Failure(Exception):
    pass


def run(argv, timeout=120):
    done = subprocess.run(argv, capture_output=True, text=True, timeout=timeout)
    return done.returncode, done.stdout, done.stderr


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--team', required=True)
    parser.add_argument('--owner-ssh', required=True)
    parser.add_argument('--machine', required=True, help='owner machine id to share into the team')
    parser.add_argument('--member-cmd', required=True)
    parser.add_argument('--member-machine', default='', help='member machine id for the approval/transfer/remove flow')
    parser.add_argument('--suspend-cmd', default='', help='command that takes SQL on stdin and runs it on the server database')
    parser.add_argument('--skip', default='', help='comma separated sections to skip: share,approval,resources,suspension')
    args = parser.parse_args()
    skip = set(filter(None, args.skip.split(',')))
    team, machine = args.team, args.machine
    member_prefix = shlex.split(args.member_cmd)
    results = []

    def owner_raw(*arguments, timeout=120):
        return run(['ssh', args.owner_ssh, ' '.join([PB] + [shlex.quote(a) for a in arguments])], timeout)

    def member_raw(*arguments, timeout=120):
        return run(member_prefix + ['bash', '-lc', ' '.join([PB] + [shlex.quote(a) for a in arguments])], timeout)

    def parse(code, out, err):
        text = out.strip() or err.strip()
        try:
            value = json.loads(text)
        except ValueError:
            return code, text
        return code, value.get('data', value) if isinstance(value, dict) else value

    def owner(*arguments):
        code, value = parse(*owner_raw(*arguments, '--json'))
        if code != 0:
            raise Failure(f'owner {" ".join(arguments)}: {value}')
        return value

    def member(*arguments):
        code, value = parse(*member_raw(*arguments, '--json'))
        if code != 0:
            raise Failure(f'member {" ".join(arguments)}: {value}')
        return value

    def refused(who, *arguments):
        """The command must fail; returns the error code the CLI reports."""
        raw = owner_raw if who == 'owner' else member_raw
        code, value = parse(*raw(*arguments, '--json'))
        if code == 0:
            raise Failure(f'{who} {" ".join(arguments)} succeeded but must be refused')
        return value.get('error', {}).get('code', '') if isinstance(value, dict) else str(value)

    def generation():
        return str(owner('team', 'get', team)['generation'])

    owner_id = owner('auth', 'status')['account']['id']
    member_id = member('auth', 'status')['account']['id']
    current = owner('team', 'get', team)
    if not any(m['account_id'] == member_id and m['active'] for m in current['members']):
        raise Failure('member account is not an active team member')

    def member_view(machine_id):
        listed = member('--workspace', team, 'machine', 'list')['machines']
        return next((m for m in listed if m['id'] == machine_id), None)

    def check(name, function):
        started = time.time()
        try:
            function()
            results.append((name, 'PASS', round(time.time() - started, 1), ''))
        except Exception as error:  # noqa: BLE001 - report, keep cleaning up
            results.append((name, 'FAIL', round(time.time() - started, 1), str(error)[:400]))

    cleanup = []

    def machine_sharing():
        owner('team', 'machine', 'share', team, machine, '--generation', generation())
        cleanup.append(lambda: owner('team', 'machine', 'unshare', team, machine, '--generation', generation()))
        if member_view(machine) is not None:
            raise Failure('sharing alone gave the member the machine')
        owner('team', 'machine', 'grant', team, machine, '--member', member_id, '--capability', 'terminal,exec', '--generation', generation())
        view = member_view(machine)
        if view is None or set(view['permissions']) != {'terminal', 'exec'}:
            raise Failure(f'member sees {view and view["permissions"]}, want exactly terminal,exec')
        owner('team', 'machine', 'grant', team, machine, '--all-members', '--capability', 'files', '--generation', generation())
        if set(member_view(machine)['permissions']) != {'terminal', 'exec', 'files'}:
            raise Failure('all-members grant did not union with the selected grant')
        owner('team', 'machine', 'grant', team, machine, '--all-members', '--capability', 'files', '--active=false', '--generation', generation())
        owner('team', 'machine', 'grant', team, machine, '--member', member_id, '--capability', 'terminal,exec', '--active=false', '--generation', generation())
        if member_view(machine) is not None:
            raise Failure('revoked grants still show the machine to the member')
        owner('team', 'machine', 'unshare', team, machine, '--generation', generation())
        cleanup.pop()
        if member_view(machine) is not None:
            raise Failure('unshared machine is still visible to the member')
        listed = owner('machine', 'list')['machines']
        if not any(m['id'] == machine and m.get('ownership') == 'personal' for m in listed):
            raise Failure('unsharing took the machine away from its owner')

    def member_machine_flow():
        if not args.member_machine:
            raise Failure('--member-machine is required for the approval flow')
        mm = args.member_machine
        member('team', 'machine', 'share', team, mm, '--generation', str(member('team', 'get', team)['generation']))
        pending = owner('team', 'machine', 'requests', team)
        if not any(r.get('machine_id') == mm for r in (pending if isinstance(pending, list) else pending.get('requests', []))):
            raise Failure(f'a member-shared machine is not waiting for approval: {pending}')
        if any(m['id'] == mm for m in owner('--workspace', team, 'machine', 'list')['machines']):
            raise Failure('an unapproved machine is visible in the team')
        owner('team', 'machine', 'approve', team, mm, '--generation', generation())
        if not any(m['machine_id'] == mm and m['active'] for m in owner('team', 'get', team)['machines']):
            raise Failure('an approved machine is not active in the team')
        member('team', 'machine', 'unshare', team, mm, '--generation', str(member('team', 'get', team)['generation']))
        member('team', 'machine', 'share', team, mm, '--generation', str(member('team', 'get', team)['generation']))
        owner('team', 'machine', 'decline', team, mm, '--generation', generation())
        if any(m['machine_id'] == mm and m['active'] for m in owner('team', 'get', team)['machines']):
            raise Failure('a declined machine is active in the team')

    def resources():
        tunnel = None
        code, value = parse(*owner_raw('tunnel', 'create', f'lane-b-f12-{int(time.time())}', '--port', '8123', '--json'))
        if isinstance(value, dict):
            tunnel = (value.get('error') or {}).get('target') or value.get('id') or value.get('tunnel_id')
        if not tunnel:
            raise Failure(f'could not create the throwaway tunnel: {value}')
        cleanup.append(lambda: delete_tunnel(tunnel))
        before = refused('owner', 'team', 'grant', team, member_id, 'tunnel', tunnel, 'use', '--generation', generation())
        if before == '':
            raise Failure('a grant on an unattached tunnel gave no error code')
        owner('team', 'attach', team, 'tunnel', tunnel, '--generation', generation())
        owner('team', 'grant', team, member_id, 'tunnel', tunnel, 'use', '--generation', generation())
        seen = member('--workspace', team, 'tunnel', 'list')
        items = seen if isinstance(seen, list) else seen.get('items') or seen.get('tunnels') or []
        if not any(t.get('id') == tunnel for t in items):
            raise Failure(f'the member does not see the attached, granted tunnel: {seen}')
        owner('team', 'grant', team, member_id, 'tunnel', tunnel, 'use', '--active=false', '--generation', generation())
        seen = member('--workspace', team, 'tunnel', 'list')
        items = seen if isinstance(seen, list) else seen.get('items') or seen.get('tunnels') or []
        if any(t.get('id') == tunnel for t in items):
            raise Failure('a revoked tunnel grant still shows the tunnel')
        owner('team', 'attach', team, 'tunnel', tunnel, '--active=false', '--generation', generation())

    def delete_tunnel(tunnel):
        code, value = parse(*owner_raw('tunnel', 'delete', tunnel, '--json'))
        token = (value.get('data') or {}).get('confirmation_token') if isinstance(value, dict) else None
        if token:
            owner_raw('tunnel', 'delete', tunnel, '--confirm', token, '--json')

    def activity():
        feed = owner('team', 'activity', team, '--limit', '100')
        kinds = {e['action'] for e in feed['items']}
        for needed in ('machine_share', 'machine_grant', 'machine_unshare'):
            if needed not in kinds:
                raise Failure(f'owner activity lacks {needed}: {sorted(kinds)}')
        code = refused('member', 'team', 'activity', team)
        if code == '':
            raise Failure('member activity refusal has no error code')

    def run_sql(sql):
        done = subprocess.run(shlex.split(args.suspend_cmd), input=sql, capture_output=True, text=True, timeout=60)
        if done.returncode != 0 or 'ERROR' in done.stderr or 'ERROR' in done.stdout:
            raise Failure(f'sql failed: {(done.stderr or done.stdout)[:200]}')

    def suspension():
        if not args.suspend_cmd:
            raise Failure('--suspend-cmd is required')
        owner('team', 'machine', 'share', team, machine, '--generation', generation())
        cleanup.append(lambda: owner('team', 'machine', 'unshare', team, machine, '--generation', generation()))
        owner('team', 'machine', 'grant', team, machine, '--member', member_id, '--capability', 'terminal', '--generation', generation())
        if member_view(machine) is None:
            raise Failure('precondition: the member should see the machine')
        run_sql(f"update paperboat.teams set suspended=true,suspended_at=now(),suspended_by=owner_account,generation=generation+1 where team_id='{team}';")
        resumed = []
        cleanup.append(lambda: run_sql(f"update paperboat.teams set suspended=false,suspended_at=null,suspended_by=null,generation=generation+1 where team_id='{team}';"))
        resumed.append(True)
        view = member('team', 'get', team)
        if view.get('suspended') is not True:
            raise Failure('a current member does not see suspended=true')
        for who in ('owner', 'member'):
            raw = owner_raw if who == 'owner' else member_raw
            code, value = parse(*raw('team', 'machine', 'grant', team, machine, '--all-members', '--capability', 'files', '--generation', str(view['generation']), '--json'))
            error = value.get('error', {}) if isinstance(value, dict) else {}
            if code == 0 or 'suspend' not in json.dumps(error).lower():
                raise Failure(f'{who} mutation during suspension did not name the suspension: {value}')
        if member_view(machine) is not None:
            raise Failure('suspended team still exposes the machine to the member')
        run_sql(f"update paperboat.teams set suspended=false,suspended_at=null,suspended_by=null,generation=generation+1 where team_id='{team}';")
        cleanup.pop()
        recovered = member_view(machine)
        if recovered is None or 'terminal' not in recovered['permissions']:
            raise Failure('access did not return after the team resumed')
        owner('team', 'machine', 'grant', team, machine, '--all-members', '--capability', 'files', '--generation', generation())
        owner('team', 'machine', 'grant', team, machine, '--all-members', '--capability', 'files', '--active=false', '--generation', generation())

    try:
        if 'share' not in skip:
            check('F12-04 share, grant, revoke, unshare (member view exact)', machine_sharing)
        if 'approval' not in skip:
            check('F12-04 member-shared machine waits for approval; approve; decline', member_machine_flow)
        if 'resources' not in skip:
            check('F12-05 attach tunnel, explicit grant, revoke, detach', resources)
        if 'share' not in skip:
            check('F12-05 owner activity lists the changes; member refused', activity)
        if 'suspension' not in skip:
            check('F12-06 suspension: named error, access fenced, recovery', suspension)
    finally:
        while cleanup:
            try:
                cleanup.pop()()
            except Exception as error:  # noqa: BLE001
                print('cleanup failed:', str(error)[:200], file=sys.stderr)
    width = max(len(r[0]) for r in results)
    for name, state, seconds, detail in results:
        print(f'{state}  {name:<{width}}  {seconds}s  {detail}')
    return 0 if all(r[1] == 'PASS' for r in results) else 1


if __name__ == '__main__':
    sys.exit(main())
