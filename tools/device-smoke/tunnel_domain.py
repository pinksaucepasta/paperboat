#!/usr/bin/env python3
"""Tunnel domains end to end (C100-C105) and the ephemeral tunnel start/stop (C089, C123).

Runs on an enrolled Linux machine. A workspace domain in the given DNS zone is added (pb domain add),
its records written through the fixtures endpoint (token file, zone name from the command line) and
verified; a durable tunnel then serves a hostname under it over HTTPS, the domain refuses removal
while used, and hostname and domain are removed. Then `pb tunnel
--ephemeral` starts a preview and `pb tunnel stop` ends it. Everything created (tunnel, domain,
DNS records) is removed on every exit path.
"""
import argparse
import json
from pathlib import Path
import secrets
import time
import urllib.error
import urllib.request

import winhost
from install_lifecycle import registration
from pairing_lifecycle import command_result, confirmed_command, invoke
from preview_lifecycle import serving

BODY = 'synthetic tunnel origin'


class FixtureDns:
    """Test-zone DNS records through the fixtures endpoint (final_fixtures.py's /dns: the server writes them with its own DNS token and only
    deletes what it created). token_file holds the endpoint's bearer token, url its base (.../v1/test-fixtures)."""

    def __init__(self, token_file, url, zone):
        self.token = Path(token_file).read_text().strip()
        self.url = url.rstrip('/')
        self.zone_name = zone

    def call(self, method, path, body=None):
        request = urllib.request.Request(
            self.url + path, method=method, data=None if body is None else json.dumps(body).encode(),
            headers={'Authorization': 'Bearer ' + self.token, 'Content-Type': 'application/json'})
        with urllib.request.urlopen(request, timeout=30) as answer:
            text = answer.read()
        return json.loads(text)['data'] if text else None

    def add_cname(self, name, value):
        return self.call('POST', '/dns', {'type': 'CNAME', 'name': name, 'content': value})['id']

    def remove(self, record_id):
        self.call('DELETE', '/dns/' + record_id)

    def exists(self, record_id):
        try:
            self.call('GET', '/dns/' + record_id)
            return True
        except urllib.error.HTTPError as error:
            if error.code == 404:
                return False
            raise


def wait(test, what, seconds):
    deadline = time.time() + seconds
    while time.time() < deadline:
        found = test()
        if found:
            return found
        time.sleep(5)
    raise AssertionError('timed out waiting for ' + what)


def fetch(url):
    try:
        with urllib.request.urlopen(url, timeout=15) as response:
            return response.status, response.read().decode()
    except urllib.error.HTTPError as error:
        return error.code, ''
    except OSError:
        return 0, ''


def ephemeral(binary, port):
    """C089 (`pb tunnel --ephemeral`) and C123 (`pb tunnel stop`)."""
    started = invoke(binary, 'tunnel', '--ephemeral', str(port), '--json', '--background', '--ttl', '10m',
                     timeout=120)
    identifier = started['id']
    try:
        assert started['state'] == 'ready' and started['persistent'] is False
        assert started['target']['address'].endswith(':%d' % port)
        endpoint = started['endpoint']
        status, body = wait(lambda: (lambda r: r if r[0] == 200 else None)(fetch(endpoint)),
                            'the ephemeral endpoint to serve', 90)
        assert body == BODY, 'ephemeral tunnel served the wrong content'
        stopped = invoke(binary, 'tunnel', 'stop', identifier, '--json', timeout=60)
        assert stopped['id'] == identifier and stopped['state'] == 'stopped'
        wait(lambda: fetch(endpoint)[0] != 200, 'the stopped endpoint to stop serving', 60)
        identifier = None
    finally:
        if identifier:
            command_result(binary, 'tunnel', 'stop', identifier, '--json', timeout=60)


def domain(binary, dns, port):
    """A workspace domain (pb domain add/verify/remove) and a tunnel hostname under it, served over HTTPS."""
    name = 'smoke-domain-' + secrets.token_hex(6)
    workspace_domain = '%s.%s' % (name, dns.zone_name)
    hostname = 'app.' + workspace_domain
    records = []
    tunnel = None
    added_domain = False

    def call(*args, timeout=90):
        return invoke(binary, 'tunnel', *args, '--json', timeout=timeout)

    try:
        wsd = invoke(binary, 'domain', 'add', workspace_domain, '--json', timeout=60)
        added_domain = True
        assert wsd['hostname'] == workspace_domain and wsd['state'] == 'pending_dns', wsd
        by_purpose = {record['purpose']: record for record in wsd['records']}
        challenge, routing = by_purpose['verification'], by_purpose['routing']
        assert challenge['name'] == '_acme-challenge.' + workspace_domain and challenge['type'] == 'CNAME'
        assert routing['name'] == '*.' + workspace_domain and routing['type'] == 'CNAME'
        records.append(dns.add_cname(challenge['name'], challenge['value']))
        # The fixtures endpoint writes no wildcard; the one hostname used gets the routing record.
        records.append(dns.add_cname(hostname, routing['value']))
        verified = invoke(binary, 'domain', 'verify', workspace_domain, '--timeout', '4m', '--json', timeout=300)
        assert verified['state'] == 'verified' and verified['certificate']['state'] == 'ready', verified

        created = call('create', name, '--port', str(port), '--duration', '2h', '--wait', '--timeout', '60s')
        tunnel = created['tunnel']['id']
        route = call('route', 'list', tunnel)['items'][0]['id']
        added = call('domain', 'add', tunnel, hostname, '--route', route)
        domain_id = added['domain']['id']
        assert added['domain']['hostname'] == hostname
        listed = call('domain', 'list', tunnel)['items']
        assert [item['id'] for item in listed] == [domain_id], 'domain list does not show the new hostname'

        def ready():
            item = call('domain', 'list', tunnel)['items'][0]
            return item if item['state'] == 'ready' and item['certificate']['state'] == 'ready' else None
        wait(ready, 'the hostname to become ready', 300)
        shown = invoke(binary, 'domain', 'show', workspace_domain, '--json', timeout=60)
        assert [use['hostname'] for use in shown['used_by']] == [hostname], 'pb domain show does not list the hostname'
        refused = command_result(binary, 'domain', 'remove', workspace_domain, '--json', timeout=60)
        assert refused.returncode != 0 and hostname in refused.stdout + refused.stderr, 'a domain in use was removed'
        url = 'https://' + hostname + '/'
        status, body = wait(lambda: (lambda r: r if r[0] == 200 else None)(fetch(url)),
                            'the custom hostname to serve over HTTPS', 180)
        assert body == BODY, 'custom hostname served the wrong content'

        confirmed_command(binary, 'tunnel', 'domain', 'remove', tunnel, hostname, timeout=90)
        assert not call('domain', 'list', tunnel)['items'], 'removed hostname is still listed'
        wait(lambda: fetch(url)[0] != 200, 'the removed hostname to stop serving', 120)
        confirmed_command(binary, 'domain', 'remove', workspace_domain, timeout=60)
        added_domain = False
        assert workspace_domain not in [d['hostname'] for d in invoke(binary, 'domain', 'list', '--json', timeout=60)['items']]
    finally:
        problems = []
        for record in records:
            try:
                dns.remove(record)
            except Exception as error:  # keep cleaning; report below
                problems.append(str(error))
        if tunnel:
            try:
                confirmed_command(binary, 'tunnel', 'delete', tunnel, timeout=90)
            except Exception as error:
                problems.append(str(error))
        if added_domain:
            try:
                confirmed_command(binary, 'domain', 'remove', workspace_domain, timeout=60)
            except Exception as error:
                problems.append(str(error))
        if problems:
            print(json.dumps({'check': 'tunnel-domain', 'cleanup': 'failed', 'problems': problems}), flush=True)
            raise RuntimeError('cleanup failed: ' + '; '.join(problems))


ROWS = {'C089': ['tunnel'], 'C100': ['tunnel', 'domain'], 'C101': ['tunnel', 'domain', 'add'],
        'C103': ['tunnel', 'domain', 'list'], 'C104': ['tunnel', 'domain', 'remove'], 'C123': ['tunnel', 'stop'],
        'C267': ['domain'], 'C268': ['domain', 'add'], 'C269': ['domain', 'list'], 'C270': ['domain', 'show'],
        'C271': ['domain', 'verify'], 'C272': ['domain', 'remove']}
BAD = ('panic', 'goroutine', 'Traceback', 'runtime error', 'nil pointer')


def help_and_errors(binary):
    """--help names the command and its usage; missing or unknown arguments fail short and clearly."""
    for row, words in ROWS.items():
        helped = command_result(binary, *words, '--help', timeout=30)
        assert helped.returncode == 0 and 'Usage:' in helped.stdout and ' '.join(words) in helped.stdout, \
            row + ': --help is missing usage or the command name'
        # No arguments: groups list their commands, the others ask for what is missing.
        bare = command_result(binary, *words, timeout=30)
        text = bare.stdout + bare.stderr
        assert not any(word in text for word in BAD) and text.strip(), row + ': bare invocation printed nothing or a trace'
        if row not in ('C089', 'C100', 'C267', 'C269'):
            assert bare.returncode != 0, row + ': accepted missing arguments'
    for row, words in (('C101', ['domain', 'add']), ('C103', ['domain', 'list']), ('C104', ['domain', 'remove'])):
        missing = command_result(binary, 'tunnel', *words, 'tunnel_zz_missing', 'missing.invalid', '--json', '--route',
                                 'rte_zz', timeout=60)
        text = missing.stdout + missing.stderr
        assert missing.returncode != 0 and text.strip() and not any(word in text for word in BAD), \
            row + ': a missing tunnel was not refused with a clear message'
    unknown = command_result(binary, 'tunnel', 'stop', 'preview_zz_missing', '--json', timeout=60)
    assert unknown.returncode != 0 and not any(word in unknown.stdout + unknown.stderr for word in BAD), \
        'C123: stopping an unknown preview was not refused cleanly'


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--fixtures-token-file')
    parser.add_argument('--fixtures-url')
    parser.add_argument('--zone')
    parser.add_argument('--help-rows-only', action='store_true')
    args = parser.parse_args()
    binary = winhost.pb_path()
    help_and_errors(binary)
    if args.help_rows_only:
        print(json.dumps({'check': 'tunnel-domain-help', 'status': 'PASS', 'rows': sorted(ROWS)}))
        return
    assert args.fixtures_token_file and args.fixtures_url and args.zone, '--fixtures-token-file, --fixtures-url and --zone are required'
    before = registration()
    dns = FixtureDns(args.fixtures_token_file, args.fixtures_url, args.zone)
    with serving(BODY) as port:
        ephemeral(binary, port)
        domain(binary, dns, port)
    assert registration() == before
    print(json.dumps({'check': 'tunnel-domain', 'status': 'PASS', 'ephemeral_start_stop': True,
                      'workspace_domain_add_verify_use_remove': True, 'served_over_https': True,
                      'help_and_errors': True, 'enrollment_preserved': True}))


if __name__ == '__main__':
    main()
