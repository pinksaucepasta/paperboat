#!/usr/bin/env python3
"""Plan, apply, and verify Paperboat-owned Sentry operational monitors."""

import argparse
import getpass
import json
import os
import pathlib
import re
import stat
import sys
import urllib.error
import urllib.parse
import urllib.request

MAX_BODY = 1_048_576
MAX_PAGES = 10
OWNED = re.compile(r"^pb-[a-z0-9][a-z0-9-]{1,62}$")
SLUG = re.compile(r"^[A-Za-z0-9_.-]{1,64}$")
ALLOWED_TOP = {"schema", "api_base", "organization", "environment", "cron_monitors", "detectors"}
CRON_KEYS = {"slug", "name", "project", "status", "producer_configured", "schedule", "checkin_margin", "max_runtime", "failure_issue_threshold", "recovery_threshold"}
DETECTOR_KEYS = {"name", "project", "enabled", "kind", "component", "threshold", "recovery_threshold"}
ENVIRONMENTS = {"production", "sentry-validation", "development"}
COMPONENTS = {"paperboat-server", "paperboat-cli", "paperboat-daemon", "paperboat-relay", "paperboat-tunnel"}


class Error(Exception):
    pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *_args, **_kwargs):
        return None


def strict_object(value, keys, where):
    if not isinstance(value, dict):
        raise Error(f"{where} must be an object")
    unknown = set(value) - keys
    if unknown:
        raise Error(f"{where} contains unsupported fields: {', '.join(sorted(unknown))}")


def safe_slug(value, where, owned=False):
    pattern = OWNED if owned else SLUG
    if not isinstance(value, str) or not pattern.fullmatch(value):
        raise Error(f"{where} is invalid")
    return value


def api_base(value):
    parsed = urllib.parse.urlsplit(value)
    loopback = parsed.scheme == "http" and parsed.hostname in ("127.0.0.1", "::1")
    sentry = parsed.scheme == "https" and parsed.hostname in ("sentry.io", "de.sentry.io", "us.sentry.io")
    if not (loopback or sentry) or parsed.username or parsed.password or parsed.query or parsed.fragment or parsed.path.rstrip("/") != "/api/0":
        raise Error("api_base must be a supported Sentry API origin ending in /api/0")
    return value.rstrip("/")


def cron_payload(item):
    strict_object(item, CRON_KEYS, "cron monitor")
    slug = safe_slug(item.get("slug"), "cron monitor slug", owned=True)
    project = safe_slug(item.get("project"), f"{slug} project")
    status = item.get("status")
    if status not in ("active", "disabled"):
        raise Error(f"{slug} status must be active or disabled")
    producer = item.get("producer_configured")
    if not isinstance(producer, bool):
        raise Error(f"{slug} producer_configured must be boolean")
    if status == "active" and not producer:
        raise Error(f"{slug} cannot be active until its schedule producer is configured")
    schedule = item.get("schedule")
    strict_object(schedule, {"type", "value", "unit"}, f"{slug} schedule")
    if schedule.get("type") != "interval" or schedule.get("unit") not in ("minute", "hour", "day", "week") or isinstance(schedule.get("value"), bool) or not isinstance(schedule.get("value"), int) or not 1 <= schedule["value"] <= 100:
        raise Error(f"{slug} schedule must be a bounded interval")
    for field in ("checkin_margin", "max_runtime", "failure_issue_threshold", "recovery_threshold"):
        if isinstance(item.get(field), bool) or not isinstance(item.get(field), int) or not 1 <= item[field] <= 1440:
            raise Error(f"{slug} {field} must be an integer from 1 through 1440")
    name = item.get("name")
    if not isinstance(name, str) or not name.startswith("pb-") or len(name) > 128:
        raise Error(f"{slug} name must be an owned pb-* name")
    return {
        "project": project,
        "slug": slug,
        "name": name,
        "status": status,
        "is_muted": False,
        "config": {
            "schedule": schedule,
            "checkin_margin": item["checkin_margin"],
            "max_runtime": item["max_runtime"],
            "timezone": "UTC",
            "failure_issue_threshold": item["failure_issue_threshold"],
            "recovery_threshold": item["recovery_threshold"],
        },
        "alert_rule": {"targets": []},
    }


def detector_payload(item, environment):
    strict_object(item, DETECTOR_KEYS, "detector")
    name = item.get("name")
    if not isinstance(name, str) or not OWNED.fullmatch(name):
        raise Error("detector name must be an owned pb-* name")
    project = safe_slug(item.get("project"), f"{name} project")
    component = item.get("component")
    if component not in COMPONENTS:
        raise Error(f"{name} component must be a fixed Paperboat component")
    threshold = item.get("threshold")
    recovery = item.get("recovery_threshold")
    if isinstance(threshold, bool) or not isinstance(threshold, int) or not 1 <= threshold <= 1_000_000:
        raise Error(f"{name} threshold must be an integer from 1 through 1000000")
    if isinstance(recovery, bool) or not isinstance(recovery, int) or not 0 <= recovery <= threshold:
        raise Error(f"{name} recovery_threshold must be between 0 and threshold")
    if not isinstance(item.get("enabled"), bool):
        raise Error(f"{name} enabled must be boolean")
    kind = item.get("kind")
    if kind == "error_count":
        source = {"aggregate": "count()", "dataset": "events", "environment": environment, "eventTypes": ["default", "error"], "query": f"component:{component}", "queryType": 0, "timeWindow": 300}
    elif kind == "span_latency_p95":
        source = {"aggregate": "p95(span.duration)", "dataset": "events_analytics_platform", "environment": environment, "eventTypes": ["trace_item_span"], "query": f"component:{component}", "queryType": 1, "timeWindow": 300}
    else:
        raise Error(f"{name} kind must be error_count or span_latency_p95")
    return project, {
        "name": name,
        "description": f"Paperboat {environment} {kind.replace('_', ' ')} for {component}.",
        "type": "metric_issue",
        "workflow_ids": [],
        "data_sources": [source],
        "config": {"detectionType": "static"},
        "condition_group": {"logicType": "any", "conditions": [
            {"type": "gt", "comparison": threshold, "conditionResult": 75},
            {"type": "lte", "comparison": recovery, "conditionResult": 0},
        ], "actions": []},
        "enabled": item["enabled"],
    }


def load_config(path):
    try:
        with open(path, "rb") as source:
            raw = source.read(MAX_BODY + 1)
    except OSError as error:
        raise Error(f"cannot read configuration: {error.strerror}") from None
    if len(raw) > MAX_BODY:
        raise Error("configuration exceeds 1048576 bytes")
    try:
        config = json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError):
        raise Error("configuration is not valid UTF-8 JSON") from None
    strict_object(config, ALLOWED_TOP, "configuration")
    if config.get("schema") != "paperboat.sentry-operations/v1":
        raise Error("configuration schema must be paperboat.sentry-operations/v1")
    environment = config.get("environment")
    if environment not in ENVIRONMENTS:
        raise Error("environment must be production, sentry-validation, or development")
    result = {"api_base": api_base(config.get("api_base")), "organization": safe_slug(config.get("organization"), "organization"), "environment": environment, "cron": {}, "detectors": {}}
    for item in config.get("cron_monitors", []):
        payload = cron_payload(item)
        if payload["slug"] in result["cron"]:
            raise Error(f"duplicate cron monitor {payload['slug']}")
        result["cron"][payload["slug"]] = payload
    for item in config.get("detectors", []):
        project, payload = detector_payload(item, environment)
        if payload["name"] in result["detectors"]:
            raise Error(f"duplicate detector {payload['name']}")
        result["detectors"][payload["name"]] = (project, payload)
    return result


def read_token(args):
    if bool(args.token_file) == bool(args.token_stdin):
        raise Error("choose exactly one of --token-file or --token-stdin")
    if args.token_file:
        descriptor = None
        try:
            descriptor = os.open(args.token_file, os.O_RDONLY | os.O_NOFOLLOW)
            metadata = os.fstat(descriptor)
            if not stat.S_ISREG(metadata.st_mode):
                raise Error("token file must be a regular file")
            if stat.S_IMODE(metadata.st_mode) & 0o077:
                raise Error("token file must not be accessible by group or others")
            with os.fdopen(descriptor, "rb", closefd=True) as source:
                descriptor = None
                raw = source.read(16385)
            if len(raw) > 16384:
                raise Error("token file exceeds 16384 bytes")
            value = raw.decode("utf-8").strip()
        except UnicodeDecodeError:
            raise Error("token file is not valid UTF-8") from None
        except OSError as error:
            raise Error(f"cannot read token file: {error.strerror}") from None
        finally:
            if descriptor is not None:
                os.close(descriptor)
    else:
        value = getpass.getpass("Sentry token: ") if sys.stdin.isatty() else sys.stdin.readline().strip()
    if not value or any(character.isspace() for character in value):
        raise Error("token is missing or invalid")
    return value


class Client:
    def __init__(self, base, token, timeout):
        self.base = base
        self.token = token
        self.timeout = timeout
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

    def request(self, method, path, body=None):
        data = None if body is None else json.dumps(body, separators=(",", ":")).encode()
        request = urllib.request.Request(self.base + path, data=data, method=method, headers={"Authorization": "Bearer " + self.token, "Accept": "application/json", "Content-Type": "application/json", "User-Agent": "paperboat-sentry-operations/1"})
        try:
            with self.opener.open(request, timeout=self.timeout) as response:
                raw = response.read(MAX_BODY + 1)
                if len(raw) > MAX_BODY:
                    raise Error("Sentry response exceeded 1048576 bytes")
                try:
                    payload = json.loads(raw) if raw else None
                except (UnicodeDecodeError, json.JSONDecodeError):
                    raise Error("Sentry returned invalid JSON") from None
                return payload, response.headers
        except urllib.error.HTTPError as error:
            if error.code in (401, 403):
                raise Error(f"Sentry denied the request (HTTP {error.code}); verify token scopes and project access") from None
            raise Error(f"Sentry request failed with HTTP {error.code}") from None
        except (urllib.error.URLError, TimeoutError):
            raise Error("Sentry request failed or timed out") from None

    def pages(self, path):
        results = []
        for _ in range(MAX_PAGES):
            payload, headers = self.request("GET", path)
            if not isinstance(payload, list):
                raise Error("Sentry list response was not an array")
            results.extend(payload)
            next_cursor = None
            for part in headers.get("Link", "").split(","):
                if 'rel="next"' in part and 'results="true"' in part:
                    match = re.search(r"[?&]cursor=([^&>]+)", part)
                    if match:
                        next_cursor = match.group(1)
            if not next_cursor:
                return results
            separator = "&" if "?" in path else "?"
            path = re.sub(r"([?&])cursor=[^&]+", "", path).rstrip("?&") + separator + "cursor=" + next_cursor
        raise Error(f"Sentry pagination exceeded {MAX_PAGES} pages")


def subset(actual, desired):
    if isinstance(desired, dict):
        return isinstance(actual, dict) and all(key in actual and subset(actual[key], value) for key, value in desired.items())
    if isinstance(desired, list):
        return isinstance(actual, list) and len(actual) == len(desired) and all(subset(a, d) for a, d in zip(actual, desired))
    return actual == desired


def detector_actual(raw):
    sources = []
    for source in raw.get("dataSources", raw.get("data_sources", [])):
        query = source.get("queryObj", {}).get("snubaQuery", source)
        query_type = query.get("queryType")
        if query_type is None:
            query_type = 0 if query.get("dataset") == "events" else 1
        normalized = {key: query.get(key) for key in ("aggregate", "dataset", "environment", "eventTypes", "query", "timeWindow")}
        if isinstance(normalized["eventTypes"], list) and all(isinstance(value, str) for value in normalized["eventTypes"]):
            normalized["eventTypes"] = sorted(normalized["eventTypes"])
        normalized["queryType"] = query_type
        sources.append(normalized)
    group = raw.get("conditionGroup", raw.get("condition_group", {}))
    return {"name": raw.get("name"), "description": raw.get("description"), "type": raw.get("type"), "workflow_ids": raw.get("workflowIds", raw.get("workflow_ids", [])), "data_sources": sources, "config": raw.get("config"), "condition_group": {"logicType": group.get("logicType"), "conditions": [{key: condition.get(key) for key in ("type", "comparison", "conditionResult")} for condition in group.get("conditions", [])], "actions": group.get("actions", [])}, "enabled": raw.get("enabled")}


def cron_actual(raw):
    config = raw.get("config", {})
    schedule = config.get("schedule")
    if isinstance(schedule, list) and len(schedule) == 2:
        schedule = {"type": config.get("schedule_type"), "value": schedule[0], "unit": schedule[1]}
    project = raw.get("project")
    if isinstance(project, dict):
        project = project.get("slug")
    return {
        "project": project,
        "slug": raw.get("slug"),
        "name": raw.get("name"),
        "status": raw.get("status"),
        "is_muted": raw.get("isMuted", raw.get("is_muted")),
        "config": {
            "schedule": schedule,
            "checkin_margin": config.get("checkin_margin"),
            "max_runtime": config.get("max_runtime"),
            "timezone": config.get("timezone"),
            "failure_issue_threshold": config.get("failure_issue_threshold"),
            "recovery_threshold": config.get("recovery_threshold"),
        },
        "alert_rule": raw.get("alertRule", raw.get("alert_rule")),
    }


def inventory(client, config):
    org = urllib.parse.quote(config["organization"], safe="")
    crons = client.pages(f"/organizations/{org}/monitors/")
    cron_by_slug = {item.get("slug"): item for item in crons if isinstance(item, dict) and isinstance(item.get("slug"), str)}
    detector_by_name = {}
    for project in sorted({project for project, _ in config["detectors"].values()}):
        encoded = urllib.parse.quote(project, safe="")
        metadata, _ = client.request("GET", f"/projects/{org}/{encoded}/")
        project_id = metadata.get("id") if isinstance(metadata, dict) else None
        if not isinstance(project_id, (str, int)):
            raise Error(f"Sentry project {project} did not return an id")
        for item in client.pages(f"/organizations/{org}/detectors/?project={urllib.parse.quote(str(project_id), safe='')}"):
            name = item.get("name") if isinstance(item, dict) else None
            if isinstance(name, str):
                if name in detector_by_name:
                    raise Error(f"duplicate detector name returned by Sentry: {name}")
                detector_by_name[name] = item
    return cron_by_slug, detector_by_name


def build_plan(client, config):
    existing_cron, existing_detectors = inventory(client, config)
    plan = {"create": [], "update": [], "reassert": [], "unchanged": [], "unmanaged_owned": [], "limitations": []}
    for slug, desired in sorted(config["cron"].items()):
        current = existing_cron.get(slug)
        comparable = {key: value for key, value in desired.items() if key != "alert_rule"}
        action = "reassert" if current is not None and subset(cron_actual(current), comparable) else ("update" if current else "create")
        entry = {"kind": "cron_monitor", "key": slug, "desired": desired}
        if action == "reassert":
            entry["reason"] = "Sentry omits cron alert targets from monitor readback; apply reasserts targets=[]"
            plan["limitations"].append(f"{slug}: notification targets cannot be verified through the monitor API")
        plan[action].append(entry)
    for name, (project, desired) in sorted(config["detectors"].items()):
        current = existing_detectors.get(name)
        action = "unchanged" if current is not None and subset(detector_actual(current), desired) else ("update" if current else "create")
        entry = {"kind": "detector", "key": name, "project": project, "desired": desired}
        if current is not None:
            detector_id = str(current.get("id", ""))
            if not detector_id.isdigit():
                raise Error(f"Sentry detector {name} returned an invalid id")
            entry["id"] = detector_id
        plan[action].append(entry)
    desired_keys = set(config["cron"]) | set(config["detectors"])
    for name in sorted((set(existing_cron) | set(existing_detectors)) - desired_keys):
        if OWNED.fullmatch(name or ""):
            plan["unmanaged_owned"].append(name)
    return plan


def apply(client, config, plan):
    org = urllib.parse.quote(config["organization"], safe="")
    for entry in plan["create"] + plan["update"] + plan["reassert"]:
        desired = entry["desired"]
        if entry["kind"] == "cron_monitor":
            path = f"/organizations/{org}/monitors/" if entry in plan["create"] else f"/organizations/{org}/monitors/{urllib.parse.quote(entry['key'], safe='')}/"
        elif entry in plan["create"]:
            path = f"/organizations/{org}/projects/{urllib.parse.quote(entry['project'], safe='')}/detectors/"
        else:
            path = f"/organizations/{org}/detectors/{urllib.parse.quote(entry['id'], safe='')}/"
        client.request("POST" if entry in plan["create"] else "PUT", path, desired)


def main(argv=None):
    parser = argparse.ArgumentParser()
    parser.add_argument("command", choices=("plan", "apply", "verify"))
    parser.add_argument("--config", required=True)
    auth = parser.add_mutually_exclusive_group(required=True)
    auth.add_argument("--token-file")
    auth.add_argument("--token-stdin", action="store_true")
    parser.add_argument("--timeout", type=float, default=10)
    args = parser.parse_args(argv)
    try:
        if not 0 < args.timeout <= 30:
            raise Error("timeout must be greater than 0 and at most 30 seconds")
        config = load_config(args.config)
        client = Client(config["api_base"], read_token(args), args.timeout)
        plan = build_plan(client, config)
        applied = None
        if args.command == "apply":
            applied = plan
            apply(client, config, plan)
            plan = build_plan(client, config)
        output = {"schema": "paperboat.sentry-operations-plan/v1", **plan} if applied is None else {"schema": "paperboat.sentry-operations-apply/v1", "applied": applied, "readback": plan}
        print(json.dumps(output, indent=2, sort_keys=True))
        if args.command in ("apply", "verify") and (plan["create"] or plan["update"]):
            raise Error("desired Sentry configuration does not match readback")
    except Error as error:
        print(f"sentry-operations: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
