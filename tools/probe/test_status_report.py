#!/usr/bin/env python3
"""Tests for status_report.py: checks against a local server, step mapping, signing, and the 'my internet is down' rule."""
import json, socket, sys, tempfile, threading, unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
import status_report as sr  # noqa: E402


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        code, body = {"/ok": (200, b"ready"), "/down": (503, b"no"), "/relay": (401, b"relay authorization required"),
                      "/tuf": (200, b'{"signed":{"expires":"2999-01-01T00:00:00Z"}}'),
                      "/tuf-naive": (200, b'{"signed":{"expires":"2999-01-01T00:00:00"}}'),
                      "/redirect": (302, b""),
                      "/tuf-old": (200, b'{"signed":{"expires":"2001-01-01T00:00:00Z"}}')}.get(self.path, (404, b""))
        self.send_response(code); self.end_headers(); self.wfile.write(body)

    def log_message(self, *args):
        pass


class Checks(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = HTTPServer(("127.0.0.1", 0), Handler)
        cls.base = f"http://127.0.0.1:{cls.server.server_port}"
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()

    def run_one(self, **check):
        return sr.run_checks([{"unit": "u", **check}])[0]

    def test_http(self):
        self.assertTrue(self.run_one(kind="http", url=self.base + "/ok", expect="ready")["ok"])
        self.assertFalse(self.run_one(kind="http", url=self.base + "/ok", expect="other")["ok"])
        self.assertEqual(self.run_one(kind="http", url=self.base + "/down")["error"], "status 503")

    def test_relay_wants_the_authorization_answer(self):
        self.assertTrue(self.run_one(kind="relay", url=self.base + "/relay")["ok"])
        self.assertFalse(self.run_one(kind="relay", url=self.base + "/down")["ok"])

    def test_release_metadata_must_not_be_expired(self):
        self.assertTrue(self.run_one(kind="tuf", url=self.base + "/tuf")["ok"])
        self.assertFalse(self.run_one(kind="tuf", url=self.base + "/tuf-old")["ok"])

    def test_naive_expiry_is_read_as_utc_not_a_crash(self):
        self.assertTrue(self.run_one(kind="tuf", url=self.base + "/tuf-naive")["ok"])

    def test_redirect_is_a_pass_only_when_the_check_says_so(self):
        self.assertFalse(self.run_one(kind="http", url=self.base + "/redirect")["ok"])
        self.assertTrue(self.run_one(kind="http", url=self.base + "/redirect", redirect=True)["ok"])

    def test_dns_failure_is_a_failed_result(self):
        r = self.run_one(kind="http", url="http://no-such-host.invalid/", resolve_all=True)
        self.assertFalse(r["ok"])

    def test_internet_ok_with_two_answers(self):
        self.assertTrue(sr.internet_ok([self.base + "/ok", self.base + "/ok"]))

    def test_tcp(self):
        self.assertTrue(self.run_one(kind="tcp", host="127.0.0.1", port=self.server.server_port)["ok"])
        s = socket.socket(); s.bind(("127.0.0.1", 0)); port = s.getsockname()[1]; s.close()
        self.assertFalse(self.run_one(kind="tcp", host="127.0.0.1", port=port)["ok"])

    def test_refused_connection_is_a_failed_result_not_an_exception(self):
        s = socket.socket(); s.bind(("127.0.0.1", 0)); port = s.getsockname()[1]; s.close()
        self.assertFalse(self.run_one(kind="http", url=f"http://127.0.0.1:{port}/")["ok"])


class Reporting(unittest.TestCase):
    def cfg(self):
        key = tempfile.NamedTemporaryFile("w", delete=False); key.write("secret\n"); key.close()
        return {"url": "http://example.test/api/probe", "vantage": "oracle", "key_file": key.name,
                "net_urls": ["http://a.test", "http://b.test"], "checks": [{"unit": "api_health", "kind": "http", "url": "http://x.test"}]}

    def test_steps_map_to_units_and_unknown_steps_are_dropped(self):
        steps = [{"step": "exec_a_to_b", "ok": True, "seconds": 2}, {"step": "origin", "ok": True, "seconds": 0},
                 {"step": "ssh_a_to_b", "ok": False, "seconds": 45, "error": "boom"}]
        got = sr.step_results(steps, {"exec_a_to_b": "exec", "ssh_a_to_b": "ssh"})
        self.assertEqual([(r["unit"], r["ok"], r["ms"]) for r in got], [("exec", True, 2000), ("ssh", False, 45000)])

    def test_signature_matches_the_worker_formula(self):
        import hashlib, hmac
        body, sig = sr.body_and_signature(self.cfg(), [{"unit": "u", "ok": True, "ms": 1}], True, now=1700000000)
        self.assertEqual(sig, hmac.new(b"secret", f"oracle\n1700000000000\n{body}".encode(), hashlib.sha256).hexdigest())

    def test_no_internet_sends_a_heartbeat_with_no_results(self):
        sent = []
        with patch.object(sr, "internet_ok", return_value=False), patch.object(sr, "run_checks", side_effect=AssertionError("must not run")), \
                patch.object(sr, "post", side_effect=lambda cfg, results, net_ok: sent.append((results, net_ok))):
            self.assertIsNone(sr.report(self.cfg()))
        self.assertEqual(sent, [([], False)])

    def test_internet_lost_during_the_run_discards_the_failures(self):
        sent = []
        with patch.object(sr, "internet_ok", side_effect=[True, False]), \
                patch.object(sr, "run_checks", return_value=[{"unit": "api_health", "ok": False, "ms": 10, "error": "x"}]), \
                patch.object(sr, "post", side_effect=lambda cfg, results, net_ok: sent.append((results, net_ok))):
            self.assertIsNone(sr.report(self.cfg()))
        self.assertEqual(sent, [([], False)])

    def test_results_are_posted_when_the_internet_works(self):
        sent = []
        with patch.object(sr, "internet_ok", return_value=True), \
                patch.object(sr, "run_checks", return_value=[{"unit": "api_health", "ok": True, "ms": 10, "error": ""}]), \
                patch.object(sr, "post", side_effect=lambda cfg, results, net_ok: sent.append((results, net_ok))):
            sr.report(self.cfg(), [{"unit": "exec", "ok": True, "ms": 5, "error": ""}])
        self.assertEqual([r["unit"] for r in sent[0][0]], ["api_health", "exec"])
        self.assertTrue(sent[0][1])

    def test_internet_needs_two_urls_configured_and_two_answers(self):
        self.assertFalse(sr.internet_ok(["http://127.0.0.1:9"]))
        self.assertFalse(sr.internet_ok(["http://127.0.0.1:9", "http://127.0.0.1:9"]))

    def test_unreadable_key_file_does_not_raise(self):
        cfg = self.cfg(); cfg["key_file"] = "/nonexistent/key"
        self.assertFalse(sr.post(cfg, [], False))


if __name__ == "__main__":
    unittest.main()
