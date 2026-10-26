#!/usr/bin/env python3
"""F14-07: a minimum supported version blocks an old official build with a clear message; custom builds are unaffected.

Runs on the host that drives the staging environment (never production) and needs:
  * two machines paired to the staging account, each a docker container on STAGING_DOCKER_HOST:
    OFFICIAL (an official release build, its version has four numeric parts) and CUSTOM (a dev build)
  * the staging operator credential (--cred: line 1 email, line 2 password) and the staging host that holds the CA
    (OPERATOR_HOST) from which the operator API is called.
Steps, each asserted:
  1. both machines work (`pb machine list`) with no floor
  2. the floor is set to the newest published release (above the official build): the official build's `machine list`, `session list` and
     `exec` are refused with "Update required: Paperboat <floor> or newer", "no longer supported" and `pb update`, while
     `update status`, `update check` and `auth status` still work (the way out)
  3. the custom build is unaffected: `machine list` works and doctor is healthy during the floor
  4. the floor is cleared (also when a step fails): the official build works again without an update
"""
import argparse
import json
from pathlib import Path
import re
import subprocess
import time
import uuid

API = 'https://api.staging.example.test:8443'
CURL = ('curl -sS --cacert ~/stg/ca/ca.pem --resolve api.staging.example.test:8443:127.0.0.1 '
        '-b {jar} -c {jar}')
WAIT_SECONDS = 180


class Operator:
    """The staging operator API, called with curl on the staging host over ssh; the password never leaves the pipe."""

    def __init__(self, host, cred, runner=subprocess.run):
        self.host, self.cred, self.runner = host, Path(cred), runner
        self.jar = '/tmp/pb-floor-%s.jar' % uuid.uuid4().hex[:8]
        self.csrf = None

    def call(self, method, path, body=None):
        headers = ''
        if method != 'GET':
            # An operator write names its target and an opaque reference, and carries an idempotency key.
            headers = (' -H "Content-Type: application/json" -H "X-CSRF-Token: %s" -H "Idempotency-Key: %s"'
                       ' -H "Operator-Target: client-update-policy" -H "Operator-Reason: f14-07-smoke-%s"') % (
                self.csrf or '', uuid.uuid4().hex, uuid.uuid4().hex[:8])
        command = "%s -X %s%s %s%s -w '\\nHTTP %%{http_code}'%s" % (
            CURL.format(jar=self.jar), method, headers, API, path, ' -d @-' if body is not None else '')
        result = self.runner(['ssh', self.host, command], input=json.dumps(body) if body is not None else None,
                             capture_output=True, text=True, timeout=60)
        text, _, status = result.stdout.rpartition('\nHTTP ')
        return int(status or 0), text

    def login(self):
        email, password = self.cred.read_text().split('\n')[:2]
        status, text = self.call('POST', '/v1/operator/login', {'email': email, 'password': password})
        if status != 200:
            raise RuntimeError('staging operator login failed (HTTP %s)' % status)
        self.csrf = json.loads(text)['data']['csrf_token']

    def policy(self):
        status, text = self.call('GET', '/v1/admin/client-update-policy')
        if status != 200:
            raise RuntimeError('reading the update policy failed (HTTP %s)' % status)
        return json.loads(text)['data']

    def set_floor(self, version, reason):
        current = self.policy()
        status, text = self.call('PUT', '/v1/admin/client-update-policy', {
            'expected_revision': current['revision'], 'minimum_version': version, 'reason': reason})
        if status != 200:
            raise RuntimeError('setting the minimum version %r failed (HTTP %s): %s' % (version, status, text[:200]))
        return json.loads(text)['data']

    def close(self):
        self.runner(['ssh', self.host, 'rm -f ' + self.jar], capture_output=True, text=True, timeout=30)


class Box:
    """One staging machine: a container on the docker host, driven with `docker exec` as its user."""

    def __init__(self, host, name, runner=subprocess.run):
        self.host, self.name, self.runner = host, name, runner

    def pb(self, *args, timeout=90):
        command = 'sudo docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 %s /home/pbt/.local/bin/pb %s' % (
            self.name, ' '.join(args))
        result = self.runner(['ssh', self.host, command], capture_output=True, text=True, timeout=timeout)
        return result.returncode, result.stdout, result.stderr


def release_key(version):
    """The sort key of an official version (four numeric parts); a dev build does not parse and is the custom case."""
    parts = version.split('.')
    if len(parts) != 4 or not all(part.isdigit() for part in parts):
        raise RuntimeError('%r is not an official four-part version' % version)
    return tuple(int(part) for part in parts)


def published_floor(box, current):
    """The floor: the newest release published for this build (the server only accepts a published version)."""
    code, out, err = box.pb('update', 'check', '--json')
    assert code == 0, 'update check failed: ' + (out + err)[:200]
    latest = json.loads(out).get('data', {}).get('latest_version', '')
    assert release_key(latest) > release_key(current), 'the official build is already on the newest release (%s)' % latest
    return latest


def version_of(box):
    code, out, _ = box.pb('--version', '--json')
    return json.loads(out)['data']['version']


def works(box, *args):
    code, out, err = box.pb(*args)
    return code == 0


def refused(box, floor, *args):
    """The command fails with the clear message: what is required, that this version is no longer supported, what to run."""
    code, out, err = box.pb(*args)
    text = out + err
    return (code != 0 and 'Update required: Paperboat %s or newer' % floor in text
            and 'no longer supported' in text and '`pb update`' in text)


def wait(condition, what):
    deadline = time.monotonic() + WAIT_SECONDS
    started = time.monotonic()
    while time.monotonic() < deadline:
        if condition():
            return round(time.monotonic() - started, 1)
        time.sleep(3)
    raise AssertionError('timed out waiting for ' + what)


def check(operator, official, custom, floor=''):
    current = version_of(official)
    floor = floor or published_floor(official, current)
    assert release_key(floor) > release_key(current), 'the floor %s must be above the official build %s' % (floor, current)
    assert not re.fullmatch(r'\d+\.\d+\.\d+\.\d+', version_of(custom)), 'the custom machine must run a dev build'
    assert operator.policy()['minimum_version'] == '', 'staging already has a floor; clear it first'
    assert works(official, 'machine', 'list', '--json') and works(custom, 'machine', 'list', '--json'), \
        'both machines must work before the floor'
    result = {'check': 'update-floor', 'status': 'PASS', 'official_version': current, 'floor': floor}
    try:
        operator.set_floor(floor, 'F14-07 smoke check on staging')
        result['seconds_to_refuse'] = wait(lambda: refused(official, floor, 'machine', 'list'),
                                           'the official build to be refused under the floor')
        refused_commands = [command for command in (('machine', 'list'), ('session', 'list'), ('exec', custom.name, '--', 'true'))
                            if refused(official, floor, *command)]
        assert len(refused_commands) == 3, 'only %s were refused under the floor' % refused_commands
        for command in (('update', 'status', '--json'), ('update', 'check', '--json'), ('auth', 'status', '--json')):
            assert works(official, *command), '`pb %s` must keep working under the floor (it is how the machine recovers)' % ' '.join(command[:2])
        assert works(custom, 'machine', 'list', '--json'), 'the custom build was refused under the floor'
        code, out, _ = custom.pb('doctor', '--json')
        assert code == 0 and json.loads(out).get('data', json.loads(out)).get('overall') == 'healthy', \
            'the custom build is not healthy under the floor'
        result['refused'] = ['machine list', 'session list', 'exec']
        result['still_allowed'] = ['update status', 'update check', 'auth status']
        result['custom_unaffected'] = True
    finally:
        operator.set_floor('', '')  # clearing takes an empty reason
    assert operator.policy()['minimum_version'] == '', 'the floor was not cleared'
    result['seconds_to_restore'] = wait(lambda: works(official, 'machine', 'list', '--json'),
                                        'the official build to work again after the floor is cleared')
    assert version_of(official) == current, 'the official build changed version during the check'
    result['floor_cleared'] = True
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--docker-host', default='pbvm3')
    parser.add_argument('--operator-host', default='pbvm4')
    parser.add_argument('--cred', required=True)
    parser.add_argument('--official', required=True, help='container name of the official build')
    parser.add_argument('--custom', required=True, help='container name of the dev build')
    parser.add_argument('--floor', default='', help='the minimum version to set (a release staging has published); default: the newest release `pb update check` reports')
    args = parser.parse_args()
    operator = Operator(args.operator_host, args.cred)
    operator.login()
    try:
        result = check(operator, Box(args.docker_host, args.official), Box(args.docker_host, args.custom), args.floor)
    finally:
        operator.close()
    print(json.dumps(result), flush=True)


if __name__ == '__main__':
    main()
