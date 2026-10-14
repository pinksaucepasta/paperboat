#!/usr/bin/env python3
import http.server
import json
import os
import pathlib
import subprocess
import tempfile
import threading
import unittest

SCRIPT = pathlib.Path(__file__).with_name("sentry-release.py")
TOKEN = "secret-token-that-must-not-leak"


class Handler(http.server.BaseHTTPRequestHandler):
    requests = []
    statuses = []

    def do_POST(self): self.handle_request()
    def do_PUT(self): self.handle_request()
    def do_GET(self):
        type(self).requests.append((self.command, self.path, None, None))
        status = type(self).statuses.pop(0) if type(self).statuses else 200
        self.send_response(status)
        self.end_headers()
        if status == 200:
            self.wfile.write(json.dumps({"release": "paperboat-server:2026.09.19.0"}).encode())

    def handle_request(self):
        body = self.rfile.read(int(self.headers["Content-Length"]))
        type(self).requests.append((self.command, self.path, self.headers.get("Authorization"), json.loads(body)))
        status = type(self).statuses.pop(0) if type(self).statuses else 200
        self.send_response(status)
        self.end_headers()

    def log_message(self, *_): pass


class Tests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls): cls.server.shutdown()

    def setUp(self):
        Handler.requests = []
        Handler.statuses = []
        self.base = [str(SCRIPT), "--api-url", f"http://127.0.0.1:{self.server.server_port}", "--timeout", "1"]

    def invoke(self, command, *extra, token=TOKEN):
        env = {**os.environ, "SENTRY_ORG": "paperboat", "SENTRY_PROJECT": "server", "SENTRY_RELEASE": "paperboat-server:2026.09.19.0", "SENTRY_AUTH_TOKEN": token}
        return subprocess.run(self.base + [command, *extra], env=env, text=True, capture_output=True)

    def test_release_associates_exact_repository_and_commit(self):
        sha = "a" * 40
        result = self.invoke("release", "--repository", "pinksaucepasta/paperboat-server", "--commit", sha, "--source-url", "https://github.com/pinksaucepasta/paperboat-server/releases/tag/2026.09.19.0")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([r[0] for r in Handler.requests], ["POST", "PUT"])
        self.assertEqual(Handler.requests[0][2], "Bearer " + TOKEN)
        self.assertEqual(Handler.requests[0][3]["refs"], [{"repository": "pinksaucepasta/paperboat-server", "commit": sha}])
        self.assertEqual(Handler.requests[1][1], "/organizations/paperboat/releases/paperboat-server%3A2026.09.19.0/")

    def test_retries_only_three_times_and_redacts_token(self):
        Handler.statuses = [503, 503, 503]
        result = self.invoke("release", "--repository", "pinksaucepasta/paperboat-server", "--commit", "b" * 40, "--source-url", "https://github.com/example/release", "--attempts", "3")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(Handler.requests), 3)
        self.assertNotIn(TOKEN, result.stderr + result.stdout)

    def test_non_retryable_error_stops_immediately(self):
        Handler.statuses = [403, 200]
        result = self.invoke("release", "--repository", "pinksaucepasta/paperboat-server", "--commit", "d" * 40, "--source-url", "https://github.com/example/release")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(Handler.requests), 1)
        self.assertIn("HTTP 403 after 1 attempt", result.stderr)
        self.assertNotIn(TOKEN, result.stderr + result.stdout)

    def test_deploy_requires_verified_rollout_and_is_distinct(self):
        denied = self.invoke("deploy", "--environment", "production")
        self.assertNotEqual(denied.returncode, 0)
        self.assertEqual(Handler.requests, [])
        accepted = self.invoke("deploy", "--environment", "production", "--readiness-url", f"http://127.0.0.1:{self.server.server_port}/readyz")
        self.assertEqual(accepted.returncode, 0, accepted.stderr)
        self.assertEqual(Handler.requests[0][1], "/readyz")
        self.assertTrue(Handler.requests[1][1].endswith("/deploys/"))

    def test_token_file_permissions_and_no_shell_interpolation(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory, "token")
            path.write_text("literal-$(id)\n", encoding="utf-8")
            path.chmod(0o600)
            result = self.invoke("release", "--repository", "pinksaucepasta/paperboat-server", "--commit", "c" * 40, "--source-url", "https://github.com/example/release", "--token-file", str(path), token="")
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(Handler.requests[0][2], "Bearer literal-$(id)")


if __name__ == "__main__": unittest.main()
