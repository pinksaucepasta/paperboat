#!/usr/bin/env python3
"""F21-01 / F21-11: the control plane stops for ten minutes and paired machines keep working.

Runs against the staging stack only (never production): three throwaway containers on HOST paired to the staging
server, then the server container is stopped for --outage-seconds and started again.

  control_outage.py setup   --binary PB --token-cmd CMD     create and pair rel-sco-a, -b, -r
  control_outage.py drill   --server-container NAME         stop the server, check, start it, check again
  control_outage.py cleanup                                  revoke the three machines by exact id, remove containers

Checks (every one is a line in the result JSON; the drill exits 1 on any FAIL):
  baseline   exec a->b and b->a work; r is refused after it signed out (a revoked machine stays refused)
  outage     a long exec each way started before the stop is still running; new exec a->b and b->a, new
             `pb ssh` a->b, start from the access rules the host holds at 30 s, 5 min and the end of the outage;
             r is still refused; pb status shows the control-plane health item
  recovery   pb status is healthy again, both long execs finished rc 0 with their output, new exec works,
             r is still refused
TOKEN_CMD prints one enrollment token on stdout for the machine name given as its last argument.
"""
import argparse, json, os, re, subprocess, sys, time
from pathlib import Path

HERE = Path(__file__).resolve().parent
PREFIX = "rel-sco"
NAMES = {"a": f"{PREFIX}-a", "b": f"{PREFIX}-b", "r": f"{PREFIX}-r"}
STATE = Path(os.environ.get("CONTROL_OUTAGE_STATE", f"/tmp/{PREFIX}-state.json"))
PB = ".local/bin/pb"


def host_run(host, script, timeout=120):
    done = subprocess.run(["ssh", "-o", "BatchMode=yes", host, script], capture_output=True, text=True, timeout=timeout)
    return done.returncode, (done.stdout + done.stderr)


def in_ct(host, ct, command, timeout=120):
    """Run a shell command as the setup user inside a container, bounded by timeout (also enforced inside)."""
    inner = f"timeout {timeout} sh -c {json.dumps(command)}"
    return host_run(host, f"sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 -w /home/pbt {ct} sh -c {json.dumps(inner)}", timeout + 30)


class Result:
    def __init__(self, redact):
        self.checks, self.redact = [], redact

    def add(self, name, ok, detail=""):
        detail = detail.replace("\n", " ")[:300]
        for word in self.redact:
            detail = detail.replace(word, "example.test")
        self.checks.append({"check": name, "status": "PASS" if ok else "FAIL", "detail": detail})
        print(("PASS " if ok else "FAIL ") + name + " " + detail, flush=True)
        return ok


def load():
    return json.loads(STATE.read_text())


def setup(args):
    ids = {}
    for key, ct in NAMES.items():
        env = dict(os.environ, TOKEN_CMD=args.token_cmd)
        subprocess.run(["sh", str(HERE.parent / "probe" / "canary-setup.sh"), args.host, ct, ct, args.binary], env=env, check=True, timeout=900)
        rc, out = in_ct(args.host, ct, f"{PB} status --json", 60)
        ids[key] = machine_id(out, ct)
    STATE.write_text(json.dumps({"host": args.host, "ids": ids}))
    print(json.dumps(ids))


def machine_id(status_json, alias):
    """The id of the machine named alias in `pb status --json` (the account lists every machine)."""
    try:
        document = json.loads(status_json[status_json.index("{"):])
    except ValueError:
        return ""
    stack = [document]
    while stack:
        node = stack.pop()
        if isinstance(node, dict):
            if node.get("alias") == alias and str(node.get("id", "")).startswith("machine_"):
                return node["id"]
            stack.extend(node.values())
        elif isinstance(node, list):
            stack.extend(node)
    return ""


def exec_ok(host, src, dst, marker, timeout=90):
    rc, out = in_ct(host, NAMES[src], f"{PB} exec {NAMES[dst]} -- echo {marker}", timeout)
    return rc == 0 and marker in out, out.strip()[-200:]


def refused(host, src, dst, timeout=90):
    rc, out = in_ct(host, NAMES[src], f"{PB} exec {NAMES[dst]} -- echo should-not-run", timeout)
    return rc != 0 and "should-not-run" not in out, out.strip()[-200:]


def eventually(fn, seconds, every=5):
    deadline, last = time.time() + seconds, (False, "")
    while time.time() < deadline:
        last = fn()
        if last[0]:
            return last
        time.sleep(every)
    return last


def health_has_outage(host):
    rc, out = in_ct(host, NAMES["a"], f"{PB} status", 60)
    return "Health:" in out and "Paperboat" in out, out.strip()[:300]


def status_healthy(host):
    rc, out = in_ct(host, NAMES["a"], f"{PB} status", 60)
    return rc == 0 and "Health:" not in out.split("\n\n")[0], out.strip()[:300]


def drill(args):
    host = load()["host"]
    res = Result(args.redact)
    # baseline
    for src, dst in (("a", "b"), ("b", "a")):
        ok, detail = eventually(lambda: exec_ok(host, src, dst, f"baseline-{src}{dst}"), 180)
        res.add(f"baseline exec {src}->{dst}", ok, detail)
    ok, detail = eventually(lambda: exec_ok(host, "r", "b", "baseline-rb"), 120)
    res.add("baseline exec r->b before sign-out", ok, detail)
    in_ct(host, NAMES["r"], f"{PB} logout", 60)
    ok, detail = eventually(lambda: refused(host, "r", "b"), 240)
    res.add("revoked machine r is refused (control plane up)", ok, detail)
    # long sessions across the outage
    long_seconds = args.outage_seconds + 120
    for src, dst in (("a", "b"), ("b", "a")):
        tag = f"long-{src}{dst}"
        in_ct(host, NAMES[src], f"rm -f /tmp/{tag}.out /tmp/{tag}.rc; nohup sh -c '{PB} exec {NAMES[dst]} -- sh -c \"sleep {long_seconds}; echo {tag}-done\" > /tmp/{tag}.out 2>&1; echo $? > /tmp/{tag}.rc' >/dev/null 2>&1 &", 30)
    time.sleep(15)
    # the outage
    stopped = time.time()
    rc, out = host_run(host, f"sudo -n docker stop -t 20 {args.server_container}", 120)
    res.add("control plane stopped", rc == 0, out.strip()[-120:])
    for label, at in (("30 s", 30), ("5 min", 300), ("end", args.outage_seconds - 20)):
        time.sleep(max(0, stopped + at - time.time()))
        for src, dst in (("a", "b"), ("b", "a")):
            ok, detail = eventually(lambda: exec_ok(host, src, dst, f"outage-{label.replace(' ', '')}-{src}{dst}"), 60, 10)
            res.add(f"outage {label}: new exec {src}->{dst}", ok, detail)
        rc, out = in_ct(host, NAMES["a"], f"{PB} ssh {NAMES['b']} -- echo outage-ssh", 90)
        res.add(f"outage {label}: new pb ssh a->b", rc == 0 and "outage-ssh" in out, out.strip()[-200:])
        ok, detail = refused(host, "r", "b")
        res.add(f"outage {label}: revoked r still refused", ok, detail)
        for tag in ("long-ab", "long-ba"):
            rc, out = in_ct(host, NAMES[tag[-2]], f"test ! -e /tmp/{tag}.rc && echo running", 30)
            res.add(f"outage {label}: {tag} still running", "running" in out, out.strip()[-80:])
        ok, detail = eventually(lambda: health_has_outage(host), 90, 10)
        res.add(f"outage {label}: pb status shows the control plane as unreachable", ok, detail)
    rc, out = in_ct(host, NAMES["a"], f"{PB} doctor", 120)
    res.add("outage: pb doctor runs and names the control plane", rc in (0, 1) and ("control" in out.lower() or "paperboat" in out.lower()), out.strip()[:200])
    # recovery
    time.sleep(max(0, stopped + args.outage_seconds - time.time()))
    rc, out = host_run(host, f"sudo -n docker start {args.server_container}", 120)
    res.add("control plane started", rc == 0, out.strip()[-120:])
    started = time.time()
    ok, detail = eventually(lambda: status_healthy(host), 180, 5)
    res.add("recovery: pb status healthy again", ok, f"{int(time.time() - started)} s after the start; {detail}")
    for src, dst in (("a", "b"), ("b", "a")):
        ok, detail = eventually(lambda: exec_ok(host, src, dst, f"recovered-{src}{dst}"), 120)
        res.add(f"recovery: new exec {src}->{dst}", ok, detail)
    ok, detail = refused(host, "r", "b")
    res.add("recovery: revoked r still refused", ok, detail)
    for tag in ("long-ab", "long-ba"):
        ok, detail = eventually(lambda: ((lambda o: (f"{tag}-done" in o[1] and "0" in in_ct(host, NAMES[tag[-2]], f"cat /tmp/{tag}.rc", 30)[1], o[1].strip()[-120:]))(in_ct(host, NAMES[tag[-2]], f"cat /tmp/{tag}.out", 30))), 240, 10)
        res.add(f"recovery: {tag} finished rc 0 with its output", ok, detail)
    out_dir = HERE / "results"
    out_dir.mkdir(exist_ok=True)
    path = out_dir / f"control-outage-{time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())}.json"
    path.write_text(json.dumps({"outage_seconds": args.outage_seconds, "checks": res.checks}, indent=1) + "\n")
    print("result:", path)
    return 0 if all(c["status"] == "PASS" for c in res.checks) else 1


def cleanup(args):
    state = load()
    for key, machine_id in state["ids"].items():
        if machine_id:
            subprocess.run(["python3", str(HERE / "cleanup_test_account.py"), "--id", machine_id, "--stale", "0 seconds", "--apply"], check=False, timeout=300)
    host_run(state["host"], "sudo -n docker rm -f " + " ".join(NAMES.values()), 120)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("phase", choices=["setup", "drill", "cleanup"])
    parser.add_argument("--host", default="ubuntu@anvit")
    parser.add_argument("--binary")
    parser.add_argument("--token-cmd")
    parser.add_argument("--server-container", default="pb-release-staging-server-1")
    parser.add_argument("--outage-seconds", type=int, default=660)
    parser.add_argument("--redact", action="append", default=[], help="host names to replace with example.test in the result")
    args = parser.parse_args()
    if args.phase == "setup":
        setup(args)
    elif args.phase == "drill":
        sys.exit(drill(args))
    else:
        cleanup(args)


if __name__ == "__main__":
    main()
