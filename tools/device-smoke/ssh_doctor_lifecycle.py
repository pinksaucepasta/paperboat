#!/usr/bin/env python3
"""Verify real SSH readiness and actionable missing-client diagnosis."""
import argparse
import json
import os

from install_lifecycle import registration
from pairing_lifecycle import command_diagnostic, command_result, invoke
import winhost


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    binary = winhost.pb_path()
    before = registration()
    ready = invoke(binary, 'ssh', 'doctor', args.target, '--json', timeout=60)
    if (ready['machine']['alias'] != args.target or not ready['openssh']['ready']
            or ready['local_configuration'] != 'ready' or ready['managed_agent'] != 'ready'
            or ready['transport'] != 'ready' or ready['target']['state'] != 'ready'
            or ready['target']['port'] <= 0 or not ready['host_identity']
            or ready['managed_keys'] != 'reconciled'):
        raise RuntimeError('SSH doctor did not establish exact configured readiness')
    with winhost.workdir('paperboat-ssh-doctor-path-') as directory:
        # Hide SSH only in this invocation; installed tools/configuration remain intact. pb.exe takes OpenSSH from
        # fixed system locations, never PATH; PAPERBOAT_OPENSSH_PATH names the client, here one that is not there.
        hidden = ({'PAPERBOAT_OPENSSH_PATH': str(directory / 'ssh.exe')} if winhost.device()
                  else dict(os.environ, PATH=str(directory)))
        result = command_result(binary, 'ssh', 'doctor', args.target, '--json', env=hidden, timeout=30)
    try:
        error = json.loads(result.stdout)['error']
        if result.returncode == 0 or 'OpenSSH' not in error['message'] or 'install' not in error['message'].lower():
            raise ValueError('missing actionable OpenSSH diagnosis')
    except (KeyError, ValueError, TypeError) as failure:
        diagnostic = command_diagnostic(result.args, result.stdout, result.stderr)
        raise RuntimeError('SSH doctor hid the missing-client step/remedy; private diagnostics: ' + str(diagnostic)) from failure
    if registration() != before:
        raise RuntimeError('SSH diagnosis changed the enrolled identity')
    print(json.dumps({'check': 'ssh-doctor', 'target': args.target, 'status': 'PASS',
                      'phases': ['real_readiness', 'missing_client_diagnosis'], 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
