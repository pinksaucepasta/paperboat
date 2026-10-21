#!/usr/bin/env python3
"""Tunnel domains end to end (C100-C105) and the ephemeral tunnel start/stop (C089, C123).

Runs on an enrolled Linux machine. A durable tunnel gets a custom domain in the given DNS zone: add,
list, instructions, the records from the instructions written through the Cloudflare API (token
file, zone name from the command line), verify, serving over HTTPS, remove. Then `pb tunnel
--ephemeral` starts a preview and `pb tunnel stop` ends it. Everything created (tunnel, domain,
DNS records) is removed on every exit path.
"""
import argparse
import json
from pathlib import Path
import secrets
import time
import urllib.request
import http.server
import functools
import tempfile
import threading

from install_lifecycle import registration
from pairing_lifecycle import command_result, confirmed_command, invoke
from preview_lifecycle import QuietHandler

BODY = 'synthetic tunnel origin'


class Cloudflare:
    def __init__(self, token_file, zone):
        self.token = Path(token_file).read_text().strip()
        self.zone_name = zone
        self.zone = self.call('GET', '/zones?name=' + zone)['result'][0]['id']

    def call(self, method, path, body=None):
        request = urllib.request.Request(
            'https://api.cloudflare.com/client/v4' + path, method=method,
            data=None if body is None else json.dumps(body).encode(),
            headers={'Authorization': 'Bearer ' + self.token, 'Content-Type': 'application/json'})
        answer = json.load(urllib.request.urlopen(request, timeout=30))
        if not answer.get('success'):
            raise RuntimeError('DNS provider call failed: %s %s' % (method, path.split('?')[0]))
        return answer

    def add_cname(self, name, value):
        record = self.call('POST', '/zones/%s/dns_records' % self.zone, {
            'type': 'CNAME', 'name': name, 'content': value, 'ttl': 300, 'proxied': False,
            'comment': 'paperboat device-smoke tunnel_domain; temporary'})
        return record['result']['id']

    def remove(self, record_id):
        self.call('DELETE', '/zones/%s/dns_records/%s' % (self.zone, record_id))


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


def domain(binary, cloudflare, port):
    name = 'smoke-domain-' + secrets.token_hex(6)
    hostname = '%s.%s' % (name, cloudflare.zone_name)
    records = []
    tunnel = None

    def call(*args, timeout=90):
        return invoke(binary, 'tunnel', *args, '--json', timeout=timeout)

    try:
        created = call('create', name, '--port', str(port), '--duration', '2h', '--wait', '--timeout', '60s')
        tunnel = created['tunnel']['id']
        route = call('route', 'list', tunnel)['items'][0]['id']

        added = call('domain', 'add', tunnel, hostname, '--route', route, '--provider', 'cloudflare')
        domain_id = added['domain']['id']
        assert added['domain']['hostname'] == hostname
        listed = call('domain', 'list', tunnel)['items']
        assert [item['id'] for item in listed] == [domain_id], 'domain list does not show the new domain'
        assert listed[0]['state'] != 'ready', 'a domain without DNS records must not be ready'

        guide = call('domain', 'instructions', tunnel, hostname)
        assert guide['hostname'] == hostname and guide['provider'] == 'cloudflare'
        assert guide['records'] and guide['records'][0]['name'] == hostname
        assert all(record['type'] == 'CNAME' and record['value'] for record in guide['records'])
        for record in guide['records']:
            records.append(cloudflare.add_cname(record['name'], record['value']))

        verified = call('domain', 'verify', tunnel, hostname, '--wait', '--timeout', '3m', timeout=240)
        assert verified['domain']['id'] == domain_id

        def ready():
            item = call('domain', 'list', tunnel)['items'][0]
            return item if item['state'] == 'ready' and item['certificate']['state'] == 'ready' else None
        item = wait(ready, 'the domain to become ready', 480)
        assert item['dns']['observed_records'], 'ready domain lists no observed DNS records'
        url = 'https://' + hostname + '/'
        status, body = wait(lambda: (lambda r: r if r[0] == 200 else None)(fetch(url)),
                            'the custom domain to serve over HTTPS', 180)
        assert body == BODY, 'custom domain served the wrong content'

        confirmed_command(binary, 'tunnel', 'domain', 'remove', tunnel, hostname, timeout=90)
        assert not call('domain', 'list', tunnel)['items'], 'removed domain is still listed'
        wait(lambda: fetch(url)[0] != 200, 'the removed domain to stop serving', 120)
    finally:
        problems = []
        for record in records:
            try:
                cloudflare.remove(record)
            except Exception as error:  # keep cleaning; report below
                problems.append(str(error))
        if tunnel:
            try:
                confirmed_command(binary, 'tunnel', 'delete', tunnel, timeout=90)
            except Exception as error:
                problems.append(str(error))
        if problems:
            print(json.dumps({'check': 'tunnel-domain', 'cleanup': 'failed', 'problems': problems}), flush=True)
            raise RuntimeError('cleanup failed: ' + '; '.join(problems))


ROWS = {'C089': ['tunnel'], 'C100': ['tunnel', 'domain'], 'C101': ['tunnel', 'domain', 'add'],
        'C102': ['tunnel', 'domain', 'instructions'], 'C103': ['tunnel', 'domain', 'list'],
        'C104': ['tunnel', 'domain', 'remove'], 'C105': ['tunnel', 'domain', 'verify'],
        'C123': ['tunnel', 'stop']}
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
        if row not in ('C089', 'C100'):
            assert bare.returncode != 0, row + ': accepted missing arguments'
    for row, words in (('C101', ['domain', 'add']), ('C102', ['domain', 'instructions']),
                       ('C103', ['domain', 'list']), ('C104', ['domain', 'remove']),
                       ('C105', ['domain', 'verify'])):
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
    parser.add_argument('--cloudflare-token-file')
    parser.add_argument('--zone')
    parser.add_argument('--help-rows-only', action='store_true')
    args = parser.parse_args()
    binary = Path.home() / '.local/bin/pb'
    help_and_errors(binary)
    if args.help_rows_only:
        print(json.dumps({'check': 'tunnel-domain-help', 'status': 'PASS', 'rows': sorted(ROWS)}))
        return
    assert args.cloudflare_token_file and args.zone, '--cloudflare-token-file and --zone are required'
    before = registration()
    cloudflare = Cloudflare(args.cloudflare_token_file, args.zone)
    with tempfile.TemporaryDirectory(prefix='paperboat-tunnel-domain-') as directory:
        (Path(directory) / 'index.html').write_text(BODY)
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0),
                    functools.partial(QuietHandler, directory=directory))
        worker = threading.Thread(target=server.serve_forever, daemon=True)
        worker.start()
        try:
            ephemeral(binary, server.server_port)
            domain(binary, cloudflare, server.server_port)
        finally:
            server.shutdown()
            server.server_close()
            worker.join(timeout=5)
    assert registration() == before
    print(json.dumps({'check': 'tunnel-domain', 'status': 'PASS', 'ephemeral_start_stop': True,
                      'domain_add_list_instructions_verify_remove': True, 'served_over_https': True,
                      'help_and_errors': True, 'enrollment_preserved': True}))


if __name__ == '__main__':
    main()
