#!/usr/bin/env python3
"""Post-launch release gate: previous release x candidate client/server/edge on isolated staging.

Pre-launch runs record incompatibilities; they never add compatibility code. Requires the
staging owner's window. Restores candidate images and clients on every exit path. Never prod.
"""
import argparse
import contextlib
import importlib.util
import io
import itertools
import json
import os
import re
from pathlib import Path
import shlex
import subprocess
import sys
import time
import tempfile
from datetime import datetime, timezone

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('staging', ROOT / 'deploy/staging.py')
staging = importlib.util.module_from_spec(spec)
spec.loader.exec_module(staging)
MACHINES = ('pb-stg-a', 'pb-stg-b')
EDGE_RECIPE = None


class RemoteFailure(RuntimeError):
    def __init__(self, code, reason=None):
        super().__init__(f'remote command exited {code}')
        self.code, self.reason = code, reason


def cells():
    return [dict(zip(('client', 'server', 'edge'), values))
            for values in itertools.product(('new', 'old'), repeat=3)]


def command(args, timeout=180):
    result = subprocess.run(['ssh', '-o', 'BatchMode=yes', staging.HOST, shlex.join(args)],
                            capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        # Captured output may contain runtime credentials. Only the step and code are evidence.
        raise RemoteFailure(result.returncode)
    return result.stdout


def invoke_pb(machine, *args, timeout=120, input=None):
    argv = ['docker', 'exec', '-i', '-u', 'pbt', '-e', 'HOME=/home/pbt',
            '-e', 'XDG_RUNTIME_DIR=/run/user/1000', '-w', '/home/pbt',
            machine, '.local/bin/pb', '--server', staging.api_url(), *args]
    return subprocess.run(['ssh', '-o', 'BatchMode=yes', staging.HOST, shlex.join(argv)],
                          input=input, capture_output=True, text=True, timeout=timeout)


def pb(machine, *args, timeout=120):
    result = invoke_pb(machine, *args, timeout=timeout)
    if result.returncode:
        raise RemoteFailure(result.returncode, error_code(result.stdout))
    return result.stdout


def error_code(output):
    """The static `error.code` of a `--json` envelope (never its message, which may carry user data), else None."""
    try:
        code = json.loads(output)['error']['code']
    except (ValueError, KeyError, TypeError):
        return None
    return code if isinstance(code, str) and re.fullmatch(r'[a-z0-9_]{1,64}', code) else None


def artifact_version(client):
    out = json.loads(command([client, 'version', '--json']))
    return out.get('data', out)['version']


def doctor_usable(machine):
    """No failed doctor check. Staging has no relay, so `pb doctor` reports the warning daemon:relay_failover (overall
    degraded) on every machine; only a failure counts there."""
    result = invoke_pb(machine, 'doctor', '--json', timeout=60)  # a degraded doctor exits 1 but still prints its checks
    report = json.loads(result.stdout)
    return not any(c.get('status') == 'fail' for c in report.get('data', report).get('checks', []))


def install(client):
    """Switch both machines to CLIENT the way a user does, `pb update --from FILE --force`: an official build's own
    `install` demands release metadata (TUF) the staging server does not publish."""
    expected = artifact_version(client)
    for machine in MACHINES:
        source = '/home/pbt/matrix-' + str(time.time_ns())
        try:
            command(['docker', 'cp', client, f'{machine}:{source}'])
            command(['docker', 'exec', machine, 'chown', 'pbt:pbt', source])
            pb(machine, 'update', '--from', source, '--force', '--json', timeout=600)
        finally:
            command(['docker', 'exec', machine, 'rm', '-f', source])
    deadline = time.monotonic() + 180
    pending = list(MACHINES)
    while pending:
        machine = pending[0]
        try:
            version = json.loads(pb(machine, 'version', '--json'))
            if version.get('data', version).get('version') == expected and doctor_usable(machine):
                pending.pop(0)
                continue
        except (RemoteFailure, ValueError, subprocess.TimeoutExpired):
            pass  # the daemon restarts during an update
        if time.monotonic() >= deadline:
            raise RuntimeError('client switch did not end ready on the requested version')
        time.sleep(4)


def edge(image):
    """Keep the node identity, private state and exact ports while replacing only its image."""
    global EDGE_RECIPE
    if EDGE_RECIPE is None:
        EDGE_RECIPE = json.loads(command(['docker', 'inspect', staging.EDGE_NAME]))[0]
    info = EDGE_RECIPE
    mount = next(m for m in info['Mounts'] if m['Destination'] == '/state')
    if not mount['Source'].endswith('/pb-release-staging/edge'):
        raise RuntimeError('edge state is outside the isolated staging directory')
    args = ['docker', 'run', '-d', '--name', staging.EDGE_NAME,
            '--restart', 'unless-stopped', '--user', '0:0', '-v', mount['Source'] + ':/state']
    for target, bindings in (info['HostConfig']['PortBindings'] or {}).items():
        for binding in bindings:
            port = int(binding['HostPort'])
            if not 18000 <= port <= 18999:
                raise RuntimeError('refusing a port outside the staging range')
            args += ['-p', (binding.get('HostIp') or '0.0.0.0') + ':' + str(port) + ':' + target]
    args += [image, *info['Config']['Cmd']]
    replacement = staging.EDGE_NAME + '-matrix-' + str(time.time_ns())
    backup = replacement + '-previous'
    # Validate image and configuration before stopping the serving container.
    args[1] = 'create'
    args.remove('-d')
    args[args.index('--name') + 1] = replacement
    command(args)
    try:
        command(['docker', 'stop', '-t', '10', staging.EDGE_NAME])
        command(['docker', 'rename', staging.EDGE_NAME, backup])
        try:
            command(['docker', 'rename', replacement, staging.EDGE_NAME])
            command(['docker', 'start', staging.EDGE_NAME])
        except Exception:
            command(['docker', 'rm', '-f', staging.EDGE_NAME])
            command(['docker', 'rename', backup, staging.EDGE_NAME])
            command(['docker', 'start', staging.EDGE_NAME])
            raise
        command(['docker', 'rm', backup])
    finally:
        with contextlib.suppress(Exception):
            command(['docker', 'rm', '-f', replacement])
    # Bound readiness; this is convergence of a new process, not a retry of a failed check.
    started = time.time()
    node = info['Config']['Cmd'][info['Config']['Cmd'].index('-node-id') + 1]
    if not re.fullmatch(r'[A-Za-z0-9_-]+', node):
        raise RuntimeError('invalid staging node identity')
    end = time.monotonic() + 45
    while time.monotonic() < end:
        query = ("SELECT count(DISTINCT m.alias) FROM control_routes r "
                 "JOIN runtime_carrier_attachments a ON a.route_id=r.id "
                 "JOIN control_tunnel_nodes n ON n.id=a.edge_node_id "
                 "JOIN user_machines m ON m.environment_id=r.environment_id "
                 f"WHERE n.id='{node}' AND n.state='ready' AND n.last_heartbeat_at>to_timestamp({started}) "
                 "AND m.alias IN ('stg-a','stg-b') AND runtime_carrier_valid(r.id,n.id,n.process_epoch,true)")
        if staging.sql(query).strip() == '2':
            return
        time.sleep(1)
    raise RuntimeError('staging edge did not start within 45 seconds')


def check(name, action):
    started = time.monotonic()
    try:
        action()
        result = {'step': name, 'ok': True}
    except (Exception, SystemExit) as error:
        result = {'step': name, 'ok': False, 'error': type(error).__name__}
        if isinstance(error, RemoteFailure):
            result['exit_code'] = error.code
            if error.reason:
                result['reason'] = error.reason
    result['seconds'] = round(time.monotonic() - started, 1)
    return result


def verify_exec(source, target):
    if pb(source, 'exec', target, '--', 'printf', 'matrix-ok') != 'matrix-ok':
        raise RuntimeError('exec did not return its exact marker')


def verify_send():
    marker = 'matrix-' + str(time.time_ns())
    source = '/home/pbt/' + marker
    try:
        command(['docker', 'exec', '-u', 'pbt', MACHINES[0], 'sh', '-c',
                 'printf %s ' + shlex.quote(marker) + ' > ' + shlex.quote(source)])
        pb(MACHINES[0], 'send', source, '--to', 'stg-b', '--json')
        inbox = pb(MACHINES[1], 'inbox', 'path').strip()
        script = ('f=$(find ' + shlex.quote(inbox) + ' -name ' + shlex.quote(marker) +
                  ' -type f -print -quit); test -n "$f" && test "$(cat "$f")" = ' +
                  shlex.quote(marker) + '; rc=$?; test -z "$f" || rm -f "$f"; exit "$rc"')
        command(['docker', 'exec', '-u', 'pbt', MACHINES[1], 'sh', '-c', script])
    finally:
        command(['docker', 'exec', '-u', 'pbt', MACHINES[0], 'rm', '-f', source])


def verify_downgrade(old, new):
    install(new)
    install(old)
    time.sleep(10)
    for machine, peer in zip(MACHINES, ('stg-b', 'stg-a')):
        if not doctor_usable(machine):
            raise RuntimeError('downgraded machine has a failed doctor check')
        verify_exec(machine, peer)


def verify_continuity(args):
    install(args.new_client)
    staging.up(args.new_server)
    stop = '/tmp/matrix-stop-' + str(time.time_ns())
    program = ("import os,time,sys\nend=time.monotonic()+900;i=0\n"
               f"while not os.path.exists({stop!r}) and time.monotonic()<end:\n"
               " print(i,flush=True);i+=1;time.sleep(0.25)\n"
               f"sys.exit(0 if os.path.exists({stop!r}) else 2)")
    argv = ['docker', 'exec', '-u', 'pbt', '-e', 'HOME=/home/pbt',
            '-e', 'XDG_RUNTIME_DIR=/run/user/1000', '-w', '/home/pbt',
            MACHINES[0], '.local/bin/pb', '--server', staging.api_url(),
            'exec', 'stg-b', '--', 'python3', '-u', '-c', program]
    with tempfile.TemporaryFile(mode='w+', encoding='utf-8') as stream:
        process = subprocess.Popen(['ssh', '-o', 'BatchMode=yes', staging.HOST, shlex.join(argv)],
                                   stdin=subprocess.DEVNULL, stdout=stream, stderr=subprocess.PIPE, text=True)
        try:
            end = time.monotonic() + 15
            while not os.pread(stream.fileno(), 2, 0).startswith(b'0\n'):
                if process.poll() is not None or time.monotonic() >= end:
                    raise RuntimeError('held exec did not start before the first replacement')
                time.sleep(0.1)
            staging.up(args.old_server)
            edge(args.old_edge)
            staging.up(args.new_server)
            edge(args.new_edge)
            if process.poll() is not None:
                raise RuntimeError('held exec ended before rolling replacements completed')
            command(['docker', 'exec', '-u', 'pbt', MACHINES[1], 'touch', stop])
            process.communicate(timeout=45)
            stream.seek(0)
            sequence = [int(line.strip()) for line in stream]
            if process.returncode or len(sequence)<2 or sequence != list(range(len(sequence))):
                raise RuntimeError('held exec lost its session or output during rolling replacements')
        finally:
            command(['docker', 'exec', '-u', 'pbt', MACHINES[1], 'touch', stop])
            try:
                process.communicate(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.communicate(timeout=10)
            command(['docker', 'exec', '-u', 'pbt', MACHINES[1], 'rm', '-f', stop])


def complete(report):
    axes = ('client', 'server', 'edge')
    expected = {tuple(c[a] for a in axes) for c in cells()}
    actual = {tuple(c.get(a) for a in axes) for c in report.get('cells', [])}
    return (len(report.get('cells', [])) == 8 and actual == expected and
            all(c.get('ok') is True for c in report['cells']) and
            {c['step'] for c in report.get('checks', [])} ==
            {'client_downgrade', 'held_session_rolling_server_and_edge'} and
            all(c.get('ok') is True for c in report['checks']) and
            {c['step'] for c in report.get('restoration', [])} ==
            {'candidate_server', 'candidate_edge', 'candidate_clients'} and
            all(c.get('ok') is True for c in report['restoration']))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('old-client', 'new-client', 'old-server', 'new-server', 'old-edge', 'new-edge', 'output'):
        parser.add_argument('--' + name, required=True)
    parser.add_argument("--window-end", required=True, help="UTC ISO deadline for starting further cells")
    args = parser.parse_args()
    window_end = datetime.fromisoformat(args.window_end).timestamp()
    # The isolated project is mandatory even when a caller accidentally exports a production SSH target.
    project = command(['docker', 'compose', '-f', '/home/ubuntu/pb-release-staging/compose.yaml',
                       '--project-directory', '/home/ubuntu/pb-release-staging', 'config', '--format', 'json'])
    if json.loads(project)['name'] != 'pb-release-staging':
        parser.error('refusing a non-staging Compose project')
    if any(not getattr(args, name).startswith('paperboat-server:stg-') for name in ('old_server', 'new_server')):
        parser.error('server images must be staging builds')
    if any(not getattr(args, name).startswith('paperboat-tunnel:stg-') for name in ('old_edge', 'new_edge')):
        parser.error('edge images must be staging builds')
    global EDGE_RECIPE
    EDGE_RECIPE = json.loads(command(['docker', 'inspect', staging.EDGE_NAME]))[0]
    versions = {name: artifact_version(getattr(args, name + '_client')) for name in ('old', 'new')}
    if versions['old'] == versions['new']:
        parser.error('old and candidate clients must have different verified versions')
    report = {'schema': 1, 'client_versions': versions, 'cells': [], 'restoration': [], 'checks': [], 'ok': False,
              'scope': 'native access (exec, send, held exec); terminal creation, preview, tunnel and QUIC need the edge on 443 and stay with the production check-edges gate; forced relay is a separate gate',
              'inputs': {k:v for k,v in vars(args).items() if k != 'output'}}
    # A broken current fixture is not a compatibility finding. Stop before changing any axis.
    report['baseline'] = [check('exec_a_to_b', lambda: verify_exec(MACHINES[0], 'stg-b')),
                          check('exec_b_to_a', lambda: verify_exec(MACHINES[1], 'stg-a')),
                          check('send', verify_send)]
    if not all(item['ok'] for item in report['baseline']):
        report['blocked'] = 'current_fixture_failed_before_matrix'
        Path(args.output).write_text(json.dumps(report, indent=2) + '\n')
        return 1
    try:
        ordered = cells()
        for cell in ordered:
            if time.time() >= window_end - 180:
                report["window_exhausted"] = True
                break
            with contextlib.redirect_stdout(io.StringIO()):
                setup = [check('client_install', lambda: install(getattr(args, cell['client'] + '_client'))),
                         check('server_switch', lambda: staging.up(getattr(args, cell['server'] + '_server'))),
                         check('edge_switch', lambda: edge(getattr(args, cell['edge'] + '_edge')))]
            results = setup
            if all(r['ok'] for r in setup):
                results += [check('exec_a_to_b', lambda: verify_exec(MACHINES[0], 'stg-b')),
                            check('exec_b_to_a', lambda: verify_exec(MACHINES[1], 'stg-a')),
                            check('send', verify_send)]
            report['cells'].append(dict(cell, checks=results, ok=all(r['ok'] for r in results)))
            Path(args.output).write_text(json.dumps(report, indent=2) + '\n')
        if time.time() < window_end - 300:
            with contextlib.redirect_stdout(io.StringIO()):
                report['checks'] = [check('client_downgrade', lambda: verify_downgrade(args.old_client, args.new_client)),
                                check('held_session_rolling_server_and_edge', lambda: verify_continuity(args))]
    finally:
        with contextlib.redirect_stdout(io.StringIO()):
            report['restoration'] = [check('candidate_server', lambda: staging.up(args.new_server)),
                                     check('candidate_edge', lambda: edge(args.new_edge)),
                                     check('candidate_clients', lambda: install(args.new_client))]
        report['ok'] = complete(report)
        Path(args.output).write_text(json.dumps(report, indent=2) + '\n')
    return 0 if report['ok'] else 1


if __name__ == '__main__':
    sys.exit(main())
