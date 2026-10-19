"""Contract of the server-first check, against a fake server and a temporary repository."""
import contextlib
import http.server
import io
import json
import subprocess
import tempfile
import threading
import unittest
from pathlib import Path

import check_server_deployed as check


def run(repo, *args):
    return subprocess.run(["git", "-C", str(repo), *args], check=True, capture_output=True, text=True).stdout.strip()


@contextlib.contextmanager
def fake_server(body, status=200):
    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            self.send_response(status if self.path == "/v1/server-version" else 404)
            self.end_headers()
            self.wfile.write(json.dumps(body).encode())

        def log_message(self, *_):
            pass

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        yield f"http://127.0.0.1:{server.server_port}"
    finally:
        server.shutdown()
        server.server_close()


class ServerFirstTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.repo = Path(self.directory.name)
        run(self.repo, "init", "-q", "-b", "main")
        run(self.repo, "config", "user.email", "t@example.test")
        run(self.repo, "config", "user.name", "t")
        self.addCleanup(self.directory.cleanup)

    def commit(self, path, text="x"):
        file = self.repo / path
        file.parent.mkdir(parents=True, exist_ok=True)
        self.count = getattr(self, "count", 0) + 1
        file.write_text(f"{text}{self.count}")
        run(self.repo, "add", "-A")
        run(self.repo, "commit", "-q", "-m", f"change {path}")
        return run(self.repo, "rev-parse", "HEAD")

    def main(self, url, *extra):
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = check.main(["--server-url", url, "--repo", str(self.repo), *extra])
        return code, out.getvalue(), err.getvalue()

    def test_client_only_changes_after_the_deployed_commit_pass(self):
        deployed = self.commit("server/a.go")
        self.commit("cli/b.go")
        with fake_server({"data": {"version": "1", "commit": deployed}}) as url:
            code, out, _ = self.main(url)
        self.assertEqual(code, 0, out)

    def test_undeployed_server_or_shared_commit_blocks_and_names_it(self):
        deployed = self.commit("server/a.go")
        self.commit("shared/wire.go")
        self.commit("server/b.go")
        with fake_server({"data": {"version": "1", "commit": deployed}}) as url:
            code, _, err = self.main(url)
            warn_code, _, warn_err = self.main(url, "--warn")
        self.assertEqual(code, 1)
        self.assertIn("2 server/shared commit", err)
        self.assertIn("change shared/wire.go", err)
        self.assertIn("change server/b.go", err)
        self.assertEqual(warn_code, 0)
        self.assertIn("WARNING", warn_err)

    def test_unreadable_or_unknown_server_fails_closed_and_warns_in_the_gate(self):
        self.commit("server/a.go")
        for body, status in [({"data": {}}, 200), ({"data": {"commit": "development"}}, 200), ({}, 404)]:
            with self.subTest(body=body, status=status), fake_server(body, status) as url:
                self.assertEqual(self.main(url)[0], 1)
                self.assertEqual(self.main(url, "--warn")[0], 0)

    def test_a_deployed_commit_missing_from_the_clone_asks_for_full_history(self):
        self.commit("server/a.go")
        with fake_server({"data": {"commit": "a" * 40}}) as url:
            code, _, err = self.main(url)
        self.assertEqual(code, 1)
        self.assertIn("fetch the full history", err)

    def test_plain_http_to_a_remote_host_is_refused(self):
        code, _, err = self.main("http://example.test")
        self.assertEqual(code, 1)
        self.assertIn("https", err)


if __name__ == "__main__":
    unittest.main()
