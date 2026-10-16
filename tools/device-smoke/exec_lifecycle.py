#!/usr/bin/env python3
"""Qualify exact remote exec streams and normal authenticated readiness commands."""
import argparse
import base64
import hashlib
import json
from pathlib import Path
import subprocess
import tempfile
import uuid
from concurrent.futures import ThreadPoolExecutor

from install_lifecycle import registration
from pairing_lifecycle import command_diagnostic, invoke


OUTPUT_BYTES = 16 << 20


def run(binary, *arguments, input=b'', expected=0):
    command = [str(binary), *arguments]
    # Bound output on disk rather than retaining an unbounded communicate buffer.
    with tempfile.TemporaryFile() as stdout, tempfile.TemporaryFile() as stderr:
        try:
            result = subprocess.run(command, input=input, stdout=stdout, stderr=stderr, timeout=90)
        except subprocess.TimeoutExpired as error:
            raise RuntimeError('exec fixture command exceeded90s') from error
        if stdout.tell() > OUTPUT_BYTES + (1 << 20) or stderr.tell() > 1 << 20:
            raise RuntimeError('exec fixture exceeded its output bound')
        stdout.seek(0); stderr.seek(0)
        output, errors = stdout.read(), stderr.read()
    if result.returncode != expected:
        diagnostic = command_diagnostic(command, output, errors)
        raise RuntimeError('remote exit status differs; private diagnostics: ' + str(diagnostic))
    return output, errors


def execute(binary, target):
    nonce = uuid.uuid4().hex
    incoming = ('PB_INPUT_' + nonce + '_λ船\n').encode() + bytes(range(256)) * 64
    digest = hashlib.sha256(incoming).hexdigest()
    argument = 'literal spaces "quotes" λ船 ' + nonce
    code = ("import hashlib,sys\n"
            "assert sys.argv[1]==" + repr(argument) + "\n"
            "assert hashlib.sha256(sys.stdin.buffer.read()).hexdigest()==" + repr(digest) + "\n"
            "sys.stdout.buffer.write(bytes(range(256))*65536)\n"
            "sys.stdout.buffer.write('PB_UNICODE_λ船\\n'.encode())\n"
            "sys.stderr.buffer.write('PB_STDERR_λ船\\n'.encode())\n"
            "raise SystemExit(37)\n")
    output, errors = run(binary, 'exec', target, '--timeout', '60s', '--', 'python3', '-c', code, argument,
                         input=incoming, expected=37)
    expected = bytes(range(256)) * 65536 + 'PB_UNICODE_λ船\n'.encode()
    if output != expected or errors != 'PB_STDERR_λ船\n'.encode():
        raise RuntimeError('exec changed exact binary stdout, Unicode or separate stderr')
    code = "import sys; print('PB_JSON_OUT'); print('PB_JSON_ERR',file=sys.stderr); raise SystemExit(19)"
    output, errors = run(binary, 'exec', target, '--timeout', '60s', '--json', '--', 'python3', '-c', code, expected=19)
    events = [json.loads(line) for line in output.splitlines()]
    if not events or any(e['version'] != 'paperboat.exec-event/v1' for e in events):
        raise RuntimeError('exec JSON event version changed')
    operations = {e['operation_id'] for e in events}
    if len(operations) != 1 or not next(iter(operations)):
        raise RuntimeError('exec events did not retain one operation identity')
    for stream, expected in [('stdout', b'PB_JSON_OUT\n'), ('stderr', b'PB_JSON_ERR\n')]:
        observed = b''.join(base64.b64decode(e['data'], validate=True)
                            for e in events if e.get('stream') == stream)
        if observed != expected:
            raise RuntimeError('exec JSON stream changed: ' + stream)
    completed = [e for e in events if e.get('state') == 'exited']
    if len(completed) != 1 or completed[0]['result']['code'] != 19:
        raise RuntimeError('exec JSON lost the final exit status')


def readiness(binary, target):
    catalog = invoke(binary, '--workspace', 'personal', 'machine', 'list', '--json')
    matches = [item for item in catalog['machines'] if target in (item['id'], item['alias'])]
    if len(matches) != 1:
        raise RuntimeError('readiness target is not one exact authorized machine')
    target_machine = matches[0]
    ping = invoke(binary, 'ping', target, '--count', '2', '--json')
    if (ping['schema'] != 'paperboat.ping/v1' or ping['machine_id'] != target_machine['id']
            or ping['sent'] != 2 or ping['received'] != 2 or ping['lost'] != 0):
        raise RuntimeError('authenticated ping did not reach the exact target twice')
    for sample in ping['samples']:
        if sample['path'] not in ('direct', 'peer_relay', 'regional_relay') or sample['connection_ms'] <= 0 or sample['lost']:
            raise RuntimeError('authenticated ping did not report its measured path and timing')
    for condition in ['runtime', 'transport', 'ssh']:
        result = invoke(binary, 'wait', target, '--for', condition, '--timeout', '30s', '--json')
        if (result['schema'] != 'paperboat.wait-result/v1' or result['condition'] != condition
                or result['outcome'] != 'ready' or result['machine']['id'] != target_machine['id']):
            raise RuntimeError('wait did not establish the exact target readiness: ' + condition)


def repeated(binary, target):
    def one(index):
        marker = 'PB_EXEC_' + uuid.uuid4().hex + '_' + str(index)
        output, errors = run(binary, 'exec', target, '--timeout', '30s', '--',
                             'printf', '%s', marker)
        if output != marker.encode() or errors:
            raise RuntimeError('short exec lost or mixed its exact output')
    for index in range(50):
        one(index)
    with ThreadPoolExecutor(max_workers=10) as pool:
        list(pool.map(one, range(10)))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--check', choices=['exec', 'readiness', 'repeated'], required=True)
    args = parser.parse_args()
    binary = Path.home() / '.local/bin/pb'
    enrolled = registration()
    {'exec': execute, 'readiness': readiness, 'repeated': repeated}[args.check](binary, args.target)
    if registration() != enrolled:
        raise RuntimeError('exec/readiness changed the enrolled identity')
    print(json.dumps({'check': args.check + '-main', 'target': args.target, 'status': 'PASS',
                      'enrollment_unchanged': True}), flush=True)


if __name__ == '__main__':
    main()
