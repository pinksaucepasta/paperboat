#!/usr/bin/env python3
"""F25-06, F25-07 and C294..C297: the team's own S3-compatible bucket and the recordings archive in it.

Usage: team_bucket_lifecycle.py TARGET --team TEAM --endpoint https://HOST --bucket NAME --s3-env FILE
           [--region us-east-1] [--vault-password-file FILE]
FILE holds AK=<access key id> and SK=<secret access key> lines (read here, never printed). TARGET is a machine of TEAM that
runs a build with the archive loop; the signed-in account is a team owner or admin who may replay. Phases (one JSON line
each; the last line is the receipt): the bucket commands refuse what they must (a private address, a wrong secret naming
its failing step, and nothing is stored), save a good bucket without ever showing the secret, keep the stored secret when
only another field changes; then a recording made on TARGET is archived (listed as archived, its objects are in the bucket and
hold ciphertext only), replays from the bucket with --archive, and `remove` disconnects it. No secret and no terminal text
is printed: only counts, ids and the marker tokens this check generates itself.
"""
import argparse
import datetime
import hashlib
import hmac
import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from xml.etree import ElementTree

import winhost
from pairing_lifecycle import command_result
import recordings_lifecycle as rec

PB = winhost.pb_path()


def bucket_cmd(team, *args, secret=None, timeout=120):
    arguments = ['team', 'bucket', *args, team, '--json']
    if secret is not None:
        arguments.append('--secret-access-key-stdin')
    return command_result(PB, *arguments, input=None if secret is None else secret + '\n', timeout=timeout)


def configured(team):
    result = bucket_cmd(team, 'get')
    return json.loads(result.stdout) if result.returncode == 0 else None


def must_not_leak(secret, *results):
    for result in results:
        if secret in (result.stdout or '') + (result.stderr or ''):
            raise RuntimeError('the secret access key was printed')


def sigv4_get(endpoint, bucket, access, secret, query, key=None, region='us-east-1'):
    """A GET with AWS Signature V4 (path style), stdlib only: this check runs on Linux, macOS and Windows controllers."""
    host = urllib.parse.urlparse(endpoint).netloc
    path = '/' + bucket + ('/' + urllib.parse.quote(key) if key else '')
    now = datetime.datetime.now(datetime.timezone.utc)
    amz, day = now.strftime('%Y%m%dT%H%M%SZ'), now.strftime('%Y%m%d')
    canonical_query = '&'.join('%s=%s' % (urllib.parse.quote(k, safe=''), urllib.parse.quote(v, safe='')) for k, v in sorted(query.items()))
    empty = hashlib.sha256(b'').hexdigest()
    canonical = '\n'.join(['GET', path, canonical_query, 'host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n' % (host, empty, amz), 'host;x-amz-content-sha256;x-amz-date', empty])
    scope = '%s/%s/s3/aws4_request' % (day, region)
    to_sign = '\n'.join(['AWS4-HMAC-SHA256', amz, scope, hashlib.sha256(canonical.encode()).hexdigest()])
    signing = ('AWS4' + secret).encode()
    for part in (day, region, 's3', 'aws4_request'):
        signing = hmac.new(signing, part.encode(), hashlib.sha256).digest()
    signature = hmac.new(signing, to_sign.encode(), hashlib.sha256).hexdigest()
    request = urllib.request.Request(endpoint + path + ('?' + canonical_query if canonical_query else ''), headers={
        'x-amz-date': amz, 'x-amz-content-sha256': empty,
        'Authorization': 'AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=%s' % (access, scope, signature)})
    with urllib.request.urlopen(request, timeout=60) as response:
        return response.read()


def objects(endpoint, bucket, access, secret, prefix, region):
    body = sigv4_get(endpoint, bucket, access, secret, {'list-type': '2', 'prefix': prefix}, region=region)
    return [e.text for e in ElementTree.fromstring(body).iter() if e.tag.endswith('Key')]


def phase_commands(args, access, secret):
    team = args.team
    bucket_cmd(team, 'remove')  # a clean start; refused when none is saved
    if configured(team) is not None:
        raise RuntimeError('the bucket is still configured after remove')
    base = ['set', '--endpoint', args.endpoint, '--bucket', args.bucket, '--region', args.region, '--access-key-id', access, '--path-style']
    private = bucket_cmd(team, 'set', '--endpoint', 'https://10.0.0.5', '--bucket', args.bucket, '--region', args.region, '--access-key-id', access, secret='x')
    if private.returncode == 0:
        raise RuntimeError('a private-address endpoint was accepted')
    wrong = bucket_cmd(team, *base, secret=secret[::-1])
    must_not_leak(secret, wrong)
    text = (wrong.stdout or '') + (wrong.stderr or '')
    if wrong.returncode == 0 or not re.search(r'\b(write|read|delete)\b', text):
        raise RuntimeError('a wrong secret was not refused naming its failing step')
    if configured(team) is not None:
        raise RuntimeError('a refused save stored a bucket')
    good = bucket_cmd(team, *base, secret=secret)
    must_not_leak(secret, good)
    if good.returncode:
        raise RuntimeError('saving the bucket failed: exit %d: %s' % (good.returncode, (good.stderr or '')[-200:]))
    config = configured(team)
    if not config or config.get('bucket') != args.bucket or config.get('access_key_id') != access or not config.get('checked_at'):
        raise RuntimeError('pb team bucket get does not show the saved bucket')
    shown = command_result(PB, 'team', 'bucket', 'get', team, timeout=60)
    must_not_leak(secret, shown)
    prefix = 'pb-check-' + uuid.uuid4().hex[:8]
    kept = bucket_cmd(team, 'set', '--endpoint', args.endpoint, '--bucket', args.bucket, '--region', args.region, '--access-key-id', access, '--path-style', '--prefix', prefix)
    if kept.returncode or (configured(team) or {}).get('prefix') != prefix:
        raise RuntimeError('changing the prefix without the secret did not keep the stored secret')
    return prefix


def phase_archive(args, access, secret, prefix, directory):
    team, target = args.team, args.target
    before = {r['id'] for r in rec.listing(team)}
    name = 'smoke-bkt-' + uuid.uuid4().hex[:10]
    session = rec.Session(target, name)
    try:
        sid = session.session_id()
        rec.recording_control('start', sid)
        rec.wait_for('REC in this client', session.saw_rec, timeout=30, interval=0.3)
        marker = session.say('BUCKET')
        rec.recording_control('stop', sid)
    finally:
        session.end()
        rec.cleanup(PB, target, name)
    recording = rec.new_recording(team, target, before)
    ended = rec.finished(team, recording['id'])
    archived = rec.wait_for('the segments in the bucket', lambda: next((r for r in rec.listing(team) if r['id'] == ended['id'] and r.get('archived_segments', 0) >= r['segments'] >= 1), None), timeout=1500, interval=10)  # a machine told 'no bucket' asks again after 10 minutes (noBucketWait), then copies oldest first
    keys = [k for k in objects(args.endpoint, args.bucket, access, secret, prefix, args.region) if ended['id'] in k]
    if len(keys) < archived['segments']:
        raise RuntimeError('the bucket holds %d objects for %d segments' % (len(keys), archived['segments']))
    for key in keys:
        raw = sigv4_get(args.endpoint, args.bucket, access, secret, {}, key=key, region=args.region)
        if marker.encode() in raw or len(raw) < 64:
            raise RuntimeError('an archived object holds plain text or is empty')
    exported = directory / ('bucket-' + uuid.uuid4().hex + '.cast')
    result = command_result(PB, 'recordings', 'export', ended['id'], '--archive', '--output', str(exported), timeout=240)
    if result.returncode or marker not in exported.read_text():
        raise RuntimeError('replay from the bucket failed: exit %d: %s' % (result.returncode, (result.stderr or '')[-200:]))
    return ended['id'], len(keys)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    parser.add_argument('--team', required=True)
    parser.add_argument('--endpoint', required=True)
    parser.add_argument('--bucket', required=True)
    parser.add_argument('--region', default='us-east-1')
    parser.add_argument('--s3-env', required=True)
    parser.add_argument('--vault-password-file')
    args = parser.parse_args()
    os.environ['PB_SMOKE_WORKSPACE'] = args.team
    values = dict(line.strip().split('=', 1) for line in open(args.s3_env) if '=' in line)
    access, secret = values['AK'], values['SK']
    rec.unlock(args.vault_password_file)
    started = time.monotonic()
    receipt = {'check': 'team-bucket', 'target': args.target, 'status': 'PASS', 'phases': []}
    try:
        prefix = phase_commands(args, access, secret)
        receipt['phases'] += ['bucket_get_set_refusals_keep_secret']
        with winhost.workdir('paperboat-bucket-') as directory:
            receipt['archived_recording'], receipt['archived_objects'] = phase_archive(args, access, secret, prefix, directory)
        receipt['phases'] += ['recordings_archived', 'objects_are_ciphertext', 'replay_from_bucket']
    finally:
        removed = bucket_cmd(args.team, 'remove')
    if removed.returncode or configured(args.team) is not None:
        raise RuntimeError('pb team bucket remove did not disconnect the bucket')
    receipt['phases'].append('bucket_removed')
    receipt['seconds'] = round(time.monotonic() - started, 1)
    print(json.dumps(receipt), flush=True)


if __name__ == '__main__':
    main()
