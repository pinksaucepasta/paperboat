"""FixtureDns (tunnel_domain.py) speaks the fixtures endpoint's /dns contract: bearer token, {"data":{"id"}} on create, 404 for a missing id, 204 on delete."""
import http.server
import json
import tempfile
import threading
import unittest
from pathlib import Path

from tunnel_domain import FixtureDns


class Endpoint(http.server.BaseHTTPRequestHandler):
    records = {}

    def reply(self, status, body=None):
        self.send_response(status)
        self.end_headers()
        if body is not None:
            self.wfile.write(json.dumps({'data': body}).encode())

    def do_POST(self):
        assert self.headers['Authorization'] == 'Bearer secret'
        made = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        assert self.path == '/v1/test-fixtures/dns' and made['type'] == 'CNAME'
        self.records['r1'] = made
        self.reply(200, {'id': 'r1', 'name': made['name']})

    def do_GET(self):
        self.reply(200, {'id': 'r1'}) if self.path.endswith('/dns/r1') and 'r1' in self.records else self.reply(404)

    def do_DELETE(self):
        self.records.pop(self.path.rsplit('/', 1)[1], None)
        self.reply(204)

    def log_message(self, *_):
        pass


class FixtureDnsTest(unittest.TestCase):
    def test_create_exists_remove(self):
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Endpoint)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        with tempfile.TemporaryDirectory() as directory:
            token = Path(directory) / 'token'
            token.write_text('secret\n')
            dns = FixtureDns(token, 'http://127.0.0.1:%d/v1/test-fixtures/' % server.server_port, 'zone.test')
            record = dns.add_cname('a.zone.test', 'edge.example.test')
            self.assertEqual(record, 'r1')
            self.assertTrue(dns.exists(record))
            dns.remove(record)
            self.assertFalse(dns.exists(record))
        server.shutdown()


if __name__ == '__main__':
    unittest.main()
