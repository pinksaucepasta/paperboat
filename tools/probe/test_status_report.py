#!/usr/bin/env python3
"""Tests for status_report.py: checks against a local server, step mapping, signing, and the 'my internet is down' rule."""
import base64, hashlib, json, socket, ssl, subprocess, sys, tempfile, threading, unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
import status_report as sr  # noqa: E402


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        code, body = {"/ok": (200, b"ready"), "/down": (503, b"no"), "/relay": (401, b"relay authorization required"),
                      "/tuf": (200, b'{"signed":{"expires":"2999-01-01T00:00:00Z"}}'),
                      "/tuf-nanos": (200, b'{"signed":{"expires":"2999-01-01T07:36:26.610746564+00:00"}}'),
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

    def test_release_metadata_must_not_be_expired(self):
        self.assertTrue(self.run_one(kind="tuf", url=self.base + "/tuf")["ok"])
        self.assertFalse(self.run_one(kind="tuf", url=self.base + "/tuf-old")["ok"])

    def test_naive_expiry_is_read_as_utc_not_a_crash(self):
        self.assertTrue(self.run_one(kind="tuf", url=self.base + "/tuf-naive")["ok"])

    def test_nanosecond_expiry_parses_on_any_python(self):
        self.assertTrue(self.run_one(kind="tuf", url=self.base + "/tuf-nanos")["ok"])

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


class PinnedRelay(unittest.TestCase):
    """A relay's certificate is private: the check must trust exactly its key, and fail closed otherwise."""

    @classmethod
    def setUpClass(cls):
        cls.dir = tempfile.mkdtemp()
        key, crt = f"{cls.dir}/k.pem", f"{cls.dir}/c.pem"
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", key, "-out", crt, "-days", "2", "-subj", "/CN=relay.example.test"], check=True, capture_output=True)
        der = subprocess.run(f"openssl x509 -in {crt} -pubkey -noout | openssl pkey -pubin -outform der", shell=True, check=True, capture_output=True).stdout
        cls.pin = "sha256//" + base64.b64encode(hashlib.sha256(der).digest()).decode()
        cls.server = HTTPServer(("127.0.0.1", 0), Handler)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER); context.load_cert_chain(crt, key)
        cls.server.socket = context.wrap_socket(cls.server.socket, server_side=True)
        cls.url = f"https://127.0.0.1:{cls.server.server_port}/relay"
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()

    def check(self, **extra):
        return sr.run_checks([{"unit": "relay_x", "kind": "relay", "url": self.url, **extra}])[0]

    def test_the_right_key_passes(self):
        got = self.check(pin=self.pin)
        self.assertTrue(got["ok"], got)

    def test_another_key_fails(self):
        self.assertFalse(self.check(pin="sha256//" + base64.b64encode(b"x" * 32).decode())["ok"])

    def test_a_relay_the_set_does_not_name_is_judged_on_validity_and_its_answer_only(self):
        got = self.check()
        self.assertTrue(got["ok"], got)

    def relay_file(self, spki_b64std, exp=4102444800):
        """A relay file like the daemon keeps: a signed candidate set (the signature is not read here) naming this relay's key."""
        raw = base64.b64decode(spki_b64std.split("//")[1])
        claims = {"exp": exp, "nodes": [{"endpoint_host": "127.0.0.1", "endpoint_tcp_port": self.server.server_port, "tls_spki_sha256": base64.urlsafe_b64encode(raw).decode().rstrip("="), "roles": ["relay"]}]}
        payload = base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip("=")
        return json.dumps({"candidate_set": f"h.{payload}.s", "relay_grants": []})

    def checked(self, file_text):
        checks = sr.with_identities([{"unit": "relay_x", "kind": "relay", "url": self.url}], sr.relay_pins(file_text))
        return sr.run_checks(checks)[0]

    def test_identity_comes_from_the_signed_set_not_from_config(self):
        got = self.checked(self.relay_file(self.pin))
        self.assertTrue(got["ok"], got)

    def test_a_rotation_follows_the_set_with_no_config_change(self):
        stale = self.relay_file("sha256//" + base64.b64encode(b"o" * 32).decode())  # the set still names the old key: the relay is not who the set says
        self.assertFalse(self.checked(stale)["ok"])
        got = self.checked(self.relay_file(self.pin))
        self.assertTrue(got["ok"], got)  # the set was refreshed with the rotated key: green again, same config

    def test_an_expired_or_unreadable_set_names_no_identity(self):
        self.assertEqual(sr.relay_pins(self.relay_file(self.pin, exp=1)), {})
        self.assertEqual(sr.relay_pins("not json"), {})
        self.assertEqual(sr.relay_pins('{"candidate_set": "x"}'), {})

    def test_a_relay_not_in_the_set_is_still_checked_for_an_expired_certificate(self):
        self.assertTrue(sr.cert_valid_now("127.0.0.1", self.server.server_port))
        self.assertFalse(sr.cert_valid_now("127.0.0.1", self.server.server_port, now=4102444800 + 10 * 365 * 86400))


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

    def test_a_failed_or_timed_out_runner_fails_every_unit_it_should_have_covered(self):
        steps = [{"step": "exec_a_to_b", "ok": True, "seconds": 1}, {"step": "probe", "ok": False, "seconds": 120, "error": "probe.sh did not finish"}]
        got = {r["unit"]: r["ok"] for r in sr.step_results(steps, {"exec_a_to_b": "exec", "ssh_a_to_b": "ssh", "send_a_to_b": "send"})}
        self.assertEqual(got, {"exec": True, "ssh": False, "send": False})

    def test_a_malformed_curl_answer_is_unreachable_not_a_crash(self):
        with patch.object(sr, "curl", side_effect=ValueError("not enough values to unpack")):
            self.assertFalse(sr.internet_ok(["http://a.test", "http://b.test"]))

    def test_a_bug_in_checking_sends_a_heartbeat_that_vouches_for_nothing(self):
        sent = []
        with patch.object(sr, "internet_ok", side_effect=RuntimeError("boom")), \
                patch.object(sr, "post", side_effect=lambda cfg, results, net_ok: sent.append((results, net_ok))):
            self.assertIsNone(sr.report(self.cfg()))
        self.assertEqual(sent, [([], False)])

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
