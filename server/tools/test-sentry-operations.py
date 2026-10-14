#!/usr/bin/env python3
import http.server
import json
import os
import pathlib
import subprocess
import tempfile
import threading
import unittest

SCRIPT = pathlib.Path(__file__).with_name("sentry-operations.py")


class Handler(http.server.BaseHTTPRequestHandler):
    requests = []
    crons = []
    detectors = []

    def reply(self, status, payload):
        data = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def record(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(length)) if length else None
        type(self).requests.append((self.command, self.path, self.headers.get("Authorization"), body))
        return body

    def do_GET(self):
        self.record()
        if self.path.endswith("/monitors/"):
            self.reply(200, type(self).crons)
        elif self.path == "/api/0/projects/paperboat/server/":
            self.reply(200, {"id": "42"})
        elif self.path == "/api/0/organizations/paperboat/detectors/?project=42":
            self.reply(200, type(self).detectors)
        else:
            self.reply(404, {})

    def do_POST(self):
        body = self.record()
        if self.path.endswith("/monitors/"):
            config = {**body["config"], "schedule_type": body["config"]["schedule"]["type"], "schedule": [body["config"]["schedule"]["value"], body["config"]["schedule"]["unit"]], "alert_rule_id": 777294}
            type(self).crons.append({"project": {"slug": body["project"]}, "slug": body["slug"], "name": body["name"], "status": body["status"], "isMuted": body["is_muted"], "config": config})
        else:
            stored = {"id": "7", **body}
            stored["data_sources"] = [{**source, "eventTypes": list(reversed(source["eventTypes"]))} for source in body["data_sources"]]
            type(self).detectors.append(stored)
        self.reply(201, body)

    def do_PUT(self):
        body = self.record()
        if "/monitors/" in self.path:
            config = {**body["config"], "schedule_type": body["config"]["schedule"]["type"], "schedule": [body["config"]["schedule"]["value"], body["config"]["schedule"]["unit"]], "alert_rule_id": 777294}
            type(self).crons = [{"project": {"slug": body["project"]}, "slug": body["slug"], "name": body["name"], "status": body["status"], "isMuted": body["is_muted"], "config": config}]
        else:
            stored = {"id": "7", **body}
            stored["data_sources"] = [{**source, "eventTypes": list(reversed(source["eventTypes"]))} for source in body["data_sources"]]
            type(self).detectors = [stored]
        self.reply(200, body)

    def log_message(self, *_): pass


class OperationsTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls): cls.server.shutdown()

    def setUp(self):
        Handler.requests = []
        Handler.crons = []
        Handler.detectors = []
        self.temp = tempfile.TemporaryDirectory()
        root = pathlib.Path(self.temp.name)
        self.token = root / "token"
        self.token.write_text("literal-$(id)\n", encoding="utf-8")
        self.token.chmod(0o600)
        self.config = root / "config.json"
        self.config.write_text(json.dumps({
            "schema": "paperboat.sentry-operations/v1",
            "api_base": f"http://127.0.0.1:{self.server.server_port}/api/0",
            "organization": "paperboat",
            "environment": "production",
            "cron_monitors": [{"slug": "pb-availability", "name": "pb-availability", "project": "server", "status": "disabled", "producer_configured": False, "schedule": {"type": "interval", "value": 1, "unit": "minute"}, "checkin_margin": 2, "max_runtime": 2, "failure_issue_threshold": 2, "recovery_threshold": 1}],
            "detectors": [{"name": "pb-server-errors", "project": "server", "enabled": True, "kind": "error_count", "component": "paperboat-server", "threshold": 10, "recovery_threshold": 2}],
        }), encoding="utf-8")

    def tearDown(self): self.temp.cleanup()

    def invoke(self, command):
        return subprocess.run([str(SCRIPT), command, "--config", str(self.config), "--token-file", str(self.token), "--timeout", "1"], text=True, capture_output=True)

    def test_plan_has_precise_creates_without_mutation(self):
        result = self.invoke("plan")
        self.assertEqual(result.returncode, 0, result.stderr)
        plan = json.loads(result.stdout)
        self.assertEqual([(x["kind"], x["key"]) for x in plan["create"]], [("cron_monitor", "pb-availability"), ("detector", "pb-server-errors")])
        self.assertFalse(any(method in ("POST", "PUT") for method, *_ in Handler.requests))
        self.assertTrue(all(auth == "Bearer literal-$(id)" for _, _, auth, _ in Handler.requests))

    def test_apply_creates_then_verifies_exact_shapes(self):
        result = self.invoke("apply")
        self.assertEqual(result.returncode, 0, result.stderr)
        output = json.loads(result.stdout)
        self.assertEqual(output["readback"]["create"], [])
        self.assertEqual(output["readback"]["update"], [])
        self.assertEqual([item["key"] for item in output["readback"]["reassert"]], ["pb-availability"])
        self.assertIn("cannot be verified", output["readback"]["limitations"][0])
        self.assertEqual(len(output["applied"]["create"]), 2)
        detector = next(body for method, path, _, body in Handler.requests if method == "POST" and path.endswith("/detectors/"))
        self.assertEqual(detector["workflow_ids"], [])
        self.assertEqual(detector["condition_group"]["actions"], [])
        self.assertEqual(detector["data_sources"][0]["aggregate"], "count()")
        self.assertEqual(detector["data_sources"][0]["eventTypes"], ["default", "error"])
        self.assertEqual(detector["data_sources"][0]["environment"], "production")
        self.assertEqual(detector["condition_group"]["conditions"][1]["comparison"], 2)
        cron = next(body for method, path, _, body in Handler.requests if method == "POST" and path.endswith("/monitors/"))
        self.assertEqual(cron["alert_rule"], {"targets": []})
        self.assertEqual(cron["status"], "disabled")

    def test_active_cron_requires_configured_producer(self):
        data = json.loads(self.config.read_text())
        data["cron_monitors"][0]["status"] = "active"
        self.config.write_text(json.dumps(data))
        result = self.invoke("plan")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("schedule producer", result.stderr)
        self.assertEqual(Handler.requests, [])

    def test_detector_recovery_and_numeric_types_are_validated(self):
        data = json.loads(self.config.read_text())
        data["detectors"][0]["recovery_threshold"] = 11
        self.config.write_text(json.dumps(data))
        result = self.invoke("plan")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("recovery_threshold", result.stderr)
        self.assertEqual(Handler.requests, [])
        data["detectors"][0]["recovery_threshold"] = 2
        data["detectors"][0]["threshold"] = True
        self.config.write_text(json.dumps(data))
        result = self.invoke("plan")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("threshold must be an integer", result.stderr)
        self.assertEqual(Handler.requests, [])

    def test_token_file_permissions_are_enforced_before_network(self):
        self.token.chmod(0o644)
        result = self.invoke("plan")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("group or others", result.stderr)
        self.assertNotIn("literal-$(id)", result.stderr + result.stdout)
        self.assertEqual(Handler.requests, [])

    def test_upstream_error_is_sanitized(self):
        original = Handler.do_GET
        def denied(handler):
            handler.record()
            handler.reply(403, {"detail": "sensitive upstream body"})
        Handler.do_GET = denied
        try:
            result = self.invoke("plan")
        finally:
            Handler.do_GET = original
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("token scopes", result.stderr)
        self.assertNotIn("sensitive upstream body", result.stderr + result.stdout)


if __name__ == "__main__": unittest.main()
