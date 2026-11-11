#!/usr/bin/env python3
"""Production canary runner (R-06): runs probe.sh once, records the result, alerts on a repeated failure.

State lives in CANARY_STATE (default ~/.local/state/paperboat-canary):
  history.jsonl  access runs only, newest last, at most HISTORY_MAX runs (24 h at one run a minute)
  account-history.jsonl  independently bounded account runs; they never replace access deploy evidence
  status.json    the last run, consecutive failures per step, red/green, and the steps already alerted
  alerts.log     every alert and recovery line, also sent to the coordinator thread (CANARY_ALERT_THREAD)
A step that fails in two runs in a row alerts once; its first pass afterwards sends one recovery line.
The probe's environment (RUN_A, CANARY_A, ...) comes from the unit's EnvironmentFile.
"""
import fcntl, json, os, subprocess, sys, time
from contextlib import contextmanager
from datetime import datetime, timezone
from pathlib import Path

HERE = Path(__file__).resolve().parent
STATE = Path(os.environ.get("CANARY_STATE", Path.home() / ".local/state/paperboat-canary"))
HISTORY_MAX = 1440
ALERT_AFTER = 2
# The account probe (sign-in, pair, device login, cleanup, sign-out) is `canary.py --account`, run by its own timer every hour
# (deploy/paperboat-canary-account.timer): WorkOS rate-limits sign-ins and mails new-device alerts, and a 2-minute run must not delay the
# one-minute probes. It runs once per interval, so its steps alert on the first failure.
ACCOUNT_STEPS = {"signin", "pair", "login", "cleanup", "signout", "account"}


def now():
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def parse(stdout, rc):
    """Steps from the probe's JSON lines. A run that printed no step (the runner itself broke) is one
    failed 'probe' step, so it can never read as green."""
    steps = []
    for line in stdout.splitlines():
        try:
            item = json.loads(line)
        except ValueError:
            continue
        if isinstance(item, dict) and isinstance(item.get("step"), str) and isinstance(item.get("ok"), bool):
            steps.append(item)
    if not steps:
        steps = [{"step": "probe", "ok": False, "seconds": 0, "error": f"probe printed no result (exit {rc})"}]
    elif rc != 0 and all(s["ok"] for s in steps):
        steps.append({"step": "probe", "ok": False, "seconds": 0, "error": f"probe exited {rc} after passing steps"})
    return steps


def advance(status, run):
    """New status from the previous one and this run, plus the alert lines to send."""
    counts = dict(status.get("consecutive_failures", {}))
    alerted = set(status.get("alerted", []))
    lines = []
    for s in run["steps"]:
        name = s["step"]
        if s["ok"]:
            counts.pop(name, None)
            if name in alerted:
                alerted.discard(name)
                lines.append(f"canary RECOVERED: {name} passes again ({run['started']})")
        else:
            counts[name] = counts.get(name, 0) + 1
            if counts[name] >= (1 if name in ACCOUNT_STEPS else ALERT_AFTER) and name not in alerted:
                alerted.add(name)
                lines.append(f"canary RED: {name} failed {counts[name]} runs in a row "
                             f"(last {run['started']}): {s.get('error', '')[:200]}")
    red = bool(counts)
    since = status.get("red_since") if red and status.get("red") else (run["started"] if red else None)
    account = run.get("group") == "account"
    new = {"updated": status.get("updated") if account else run["finished"],
           "last": status.get("last") if account else run,
           "account_last": run if account else status.get("account_last"),
           "red": red, "red_since": since,
           "consecutive_failures": counts, "alerted": sorted(alerted)}
    return new, lines


@contextmanager
def record_lock():
    """Both systemd units update one aggregate state; hold a bounded inter-process lock."""
    with (STATE / "record.lock").open("a") as lock:
        end = time.monotonic() + 15
        while True:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.monotonic() >= end:
                    raise TimeoutError("canary state writer stayed locked for 15 seconds")
                time.sleep(0.05)
        try:
            yield
        finally:
            fcntl.flock(lock, fcntl.LOCK_UN)


def record(run, alert):
    STATE.mkdir(parents=True, exist_ok=True)
    with record_lock():
        account = run.get("group") == "account"
        history = STATE / ("account-history.jsonl" if account else "history.jsonl")
        lines = history.read_text().splitlines() if history.exists() else []
        lines = (lines + [json.dumps(run)])[-HISTORY_MAX:]
        write(history, "\n".join(lines) + "\n")
        status_path = STATE / "status.json"
        try:
            status = json.loads(status_path.read_text())
        except (OSError, ValueError):
            status = {}
        status, messages = advance(status, run)
        write(status_path, json.dumps(status, indent=1) + "\n")
        if messages:
            with open(STATE / "alerts.log", "a") as log:
                log.write("".join(m + "\n" for m in messages))
    if messages:  # network delivery cannot hold the state lock or delay the other unit
        alert("\n".join(messages))
    return status


def write(path, text):  # atomic, so the status page and the deploy gate never read half a file
    tmp = path.with_suffix(".tmp")
    tmp.write_text(text)
    tmp.replace(path)


def send_alert(message):
    thread = os.environ.get("CANARY_ALERT_THREAD")
    if not thread:
        return
    try:  # the alerts.log line is already written; a failed send must not fail the run
        subprocess.run(["t3", "thread", "send", thread, message], timeout=30, capture_output=True)
    except (OSError, subprocess.TimeoutExpired):
        pass


def account_steps():
    try:
        done = subprocess.run([sys.executable, str(HERE / "account.py")], capture_output=True, text=True, timeout=420)
        stdout, rc = done.stdout, done.returncode
    except subprocess.TimeoutExpired as error:
        out = error.stdout or ""
        stdout, rc = out.decode(errors="replace") if isinstance(out, bytes) else out, 124
    steps = parse(stdout, rc)
    if len(steps) == 1 and steps[0]["step"] == "probe":  # the runner itself broke: name it for the account probe
        steps[0]["step"] = "account"
    return steps


def main():
    started = now()
    clock = time.monotonic()
    if "--account" in sys.argv[1:]:
        steps = account_steps()
    else:
        try:
            done = subprocess.run([str(HERE / "probe.sh")], capture_output=True, text=True, timeout=330)
            stdout, rc = done.stdout, done.returncode
        except subprocess.TimeoutExpired as error:  # its stdout may be bytes even with text=True
            out = error.stdout or ""
            stdout, rc = out.decode(errors="replace") if isinstance(out, bytes) else out, 124
        steps = parse(stdout, rc)
    run = {"started": started, "finished": now(), "seconds": round(time.monotonic() - clock, 1),
           "ok": all(s["ok"] for s in steps), "steps": steps,
           "group": "account" if "--account" in sys.argv[1:] else "access"}
    record(run, send_alert)
    print(json.dumps(run))
    return 0 if run["ok"] else 1


if __name__ == "__main__":
    sys.exit(main())
