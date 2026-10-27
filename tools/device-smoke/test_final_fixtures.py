import io
import json
import os
import unittest
from contextlib import redirect_stdout
from unittest import mock

import final_fixtures


class FinalFixturesTest(unittest.TestCase):
    def setUp(self):
        patcher = mock.patch.dict(os.environ, {"FINAL_FIXTURES_URL": "https://api.example.test/v1/test-fixtures", "FINAL_FIXTURES_TOKEN": "SECRET-TOKEN"})
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_create_prints_the_env_line_and_sends_the_documented_body(self):
        sent = []
        def fake(method, path, body=None, timeout=60):
            sent.append((method, path, body))
            return 200, {"data": {"env": {"PAPERBOAT_SMOKE_TEAM": "paperboat-smoke-abc"}}}
        out = io.StringIO()
        with mock.patch.object(final_fixtures, "call", fake), redirect_stdout(out):
            self.assertEqual(final_fixtures.main(["--email", "a@example.test", "--member", "b@example.test"]), 0)
        self.assertEqual(json.loads(out.getvalue()), {"env": {"PAPERBOAT_SMOKE_TEAM": "paperboat-smoke-abc"}})
        self.assertEqual(sent, [("POST", "/team", {"email": "a@example.test", "members": ["b@example.test"]})])

    def test_waits_for_the_first_sign_in_then_reports_the_code(self):
        answers = iter([(404, {"error": {"code": "account_not_found"}}), (200, {"data": {"env": {"PAPERBOAT_SMOKE_TEAM": "t"}}})])
        with mock.patch.object(final_fixtures, "call", lambda *a, **k: next(answers)), mock.patch.object(final_fixtures.time, "sleep", lambda s: None), redirect_stdout(io.StringIO()):
            self.assertEqual(final_fixtures.main(["--email", "a@example.test"]), 0)
        with mock.patch.object(final_fixtures, "call", lambda *a, **k: (404, {"error": {"code": "account_not_found"}})), mock.patch.object(final_fixtures.time, "sleep", lambda s: None):
            with self.assertRaises(SystemExit) as raised:
                final_fixtures.main(["--email", "a@example.test", "--wait", "0"])
        self.assertIn("account_not_found", str(raised.exception))

    def test_teardown_prints_what_the_server_reports(self):
        out = io.StringIO()
        with mock.patch.object(final_fixtures, "call", lambda *a, **k: (200, {"data": {"team": "t", "active_members": 1}})), redirect_stdout(out):
            self.assertEqual(final_fixtures.main(["--email", "a@example.test", "--teardown"]), 0)
        self.assertEqual(json.loads(out.getvalue()), {"teardown": {"team": "t", "active_members": 1}})

    def test_dns_helpers_use_the_dns_paths(self):
        calls = []
        def fake(method, path, body=None, timeout=60):
            calls.append((method, path, body))
            return (200, {"data": {"id": "a" * 32}}) if method == "POST" else (204, None)
        with mock.patch.object(final_fixtures, "call", fake):
            record = final_fixtures.dns_fixture("CNAME", "x.smoke.example.test", "edge.example.test")
            final_fixtures.dns_remove(record)
        self.assertEqual(calls, [("POST", "/dns", {"type": "CNAME", "name": "x.smoke.example.test", "content": "edge.example.test"}), ("DELETE", "/dns/" + "a" * 32, None)])

    def test_missing_configuration_and_plain_http_are_refused_without_echoing_the_token(self):
        with mock.patch.dict(os.environ, {"FINAL_FIXTURES_URL": "http://insecure.example.test"}):
            with self.assertRaises(SystemExit) as raised:
                final_fixtures.call("GET", "/zone")
        self.assertNotIn("SECRET-TOKEN", str(raised.exception))
        with mock.patch.dict(os.environ, {"FINAL_FIXTURES_TOKEN": ""}):
            with self.assertRaises(SystemExit):
                final_fixtures.call("GET", "/zone")

    def test_failures_name_the_status_and_code_not_the_token(self):
        with mock.patch.object(final_fixtures, "call", lambda *a, **k: (401, {"error": {"code": "test_fixtures_unauthorized"}})):
            with self.assertRaises(SystemExit) as raised:
                final_fixtures.main(["--email", "a@example.test"])
        self.assertIn("401 test_fixtures_unauthorized", str(raised.exception))
        self.assertNotIn("SECRET", str(raised.exception))


if __name__ == "__main__":
    unittest.main()
