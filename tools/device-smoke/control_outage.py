#!/usr/bin/env python3
"""F21-01 / F21-11: the control plane stops for ten minutes and paired machines keep working.

Runs against the staging stack only (never production): three throwaway containers on HOST paired to the staging
server, then the server container is stopped for --outage-seconds and started again.

  control_outage.py setup   --binary PB --token-cmd CMD     create and pair rel-sco-a, -b, -r
  control_outage.py drill   --server-container NAME         stop the server, check, start it, check again
  control_outage.py cleanup                                  revoke the three machines by exact id, remove containers

Checks (every one is a line in the result JSON; the drill exits 1 on any FAIL):
  baseline   exec a->b and b->a work; r is refused after it signed out (a revoked machine stays refused)
  outage     a long exec each way started before the stop is still running; new exec a->b and b->a, a new
             terminal (`pb connect`) a->b, new `pb ssh` a->b, start from the access rules the host holds at 30 s,
             5 min and the end of the outage;
             r is still refused; pb status shows "Control plane is unavailable" (the staging stack has no relay, so a
             "No relay is usable" warning is present throughout and is not part of the check)
  recovery   pb status is healthy again, both long execs finished rc 0 with their output, new exec works,
             r is still refused
TOKEN_CMD prints one enrollment token on stdout for the machine name given as its last argument.
"""
import argparse, base64, json, os, re, shlex, subprocess, sys, time
from pathlib import Path

HERE = Path(__file__).resolve().parent
PREFIX = "rel-sco"
NAMES = {"a": f"{PREFIX}-a", "b": f"{PREFIX}-b", "r": f"{PREFIX}-r"}
STATE = Path(os.environ.get("CONTROL_OUTAGE_STATE", f"/tmp/{PREFIX}-state.json"))
PB = ".local/bin/pb"


def host_run(host, script, timeout=120):
    done = subprocess.run(["ssh", "-o", "BatchMode=yes", host, script], capture_output=True, text=True, timeout=timeout)
    return done.returncode, (done.stdout + done.stderr)


def detached(host, ct, script):
    """Start a long-running shell script in the container and return at once (docker exec -d)."""
    return host_run(host, f"sudo -n docker exec -d -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 -w /home/pbt {ct} sh -c {shlex.quote(script)}", 60)


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


def terminal_ok(host, src, dst, marker):
    """A new `pb connect` session with the control plane unreachable: type a command and look for its result (the typed
    text shows $((6*7)); only the shell on the machine prints 42). The script goes in as base64 so no shell expands it."""
    script = f"""#!/bin/bash
(sleep 12; printf 'echo {marker}-$((6*7))\\n'; sleep 3; printf 'exit\\n'; sleep 3) | timeout 80 script -qec '{PB} connect {NAMES[dst]}' /dev/null 2>&1 | tr -d '\\r'
"""
    encoded = base64.b64encode(script.encode()).decode()
    in_ct(host, NAMES[src], f"echo {encoded} | base64 -d > /tmp/term-{marker}.sh", 30)
    rc, out = in_ct(host, NAMES[src], f"bash /tmp/term-{marker}.sh", 100)
    return f"{marker}-42" in out, out.strip()[-160:]


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
    return "Control plane is unavailable" in out, out.strip()[:300]


def status_healthy(host):
    rc, out = in_ct(host, NAMES["a"], f"{PB} status", 60)
    return rc == 0 and "Control plane is unavailable" not in out and "Daemon: ready" in out, out.strip()[:300]


def cut_control_plane(host, args):
    """--mode stop stops the staging server container (a window agreed with its owner); --mode blackhole adds a
    blackhole route to the API address inside the three containers only, the way ssh_control_outage.sh does: the
    shared server keeps running and nothing else is affected."""
    if args.mode == "stop":
        return host_run(host, f"sudo -n docker stop -t 20 {args.server_container}", 120)
    return host_run(host, " && ".join(f"sudo -n docker exec {ct} ip route add blackhole {args.api_ip}" for ct in NAMES.values()), 60)


def restore_control_plane(host, args):
    if args.mode == "stop":
        return host_run(host, f"sudo -n docker start {args.server_container}", 120)
    return host_run(host, "; ".join(f"sudo -n docker exec {ct} ip route del blackhole {args.api_ip}" for ct in NAMES.values()), 60)


def drill(args):
    host = load()["host"]
    res = Result(args.redact)
    # baseline
    for src, dst in (("a", "b"), ("b", "a")):
        ok, detail = eventually(lambda: exec_ok(host, src, dst, f"baseline-{src}{dst}"), 180)
        res.add(f"baseline exec {src}->{dst}", ok, detail)
    ok, detail = eventually(lambda: exec_ok(host, "r", "b", "baseline-rb"), 120)
    res.add("baseline exec r->b before sign-out", ok, detail)
    ok, detail = revoke_machine(host, NAMES["a"], active_name(host, NAMES["a"], NAMES["r"]))
    res.add("machine r revoked by its owner (pb machine revoke, exact enrollment)", ok, detail)
    ok, detail = eventually(lambda: refused(host, "r", "b"), 240)
    res.add("revoked machine r is refused (control plane up)", ok, detail)
    # long sessions across the outage
    long_seconds = args.outage_seconds + 120
    for src, dst in (("a", "b"), ("b", "a")):
        tag = f"long-{src}{dst}"
        detached(host, NAMES[src], f"rm -f /tmp/{tag}.out /tmp/{tag}.rc; {PB} exec {NAMES[dst]} -- sh -c 'sleep {long_seconds}; echo {tag}-done' < /dev/null > /tmp/{tag}.out 2>&1; echo $? > /tmp/{tag}.rc")
    time.sleep(15)
    # the outage
    stopped = time.time()
    rc, out = cut_control_plane(host, args)
    res.add("control plane unreachable", rc == 0, out.strip()[-120:])
    restored = None
    try:
        outage_checks(host, res, args, stopped)
    finally:
        restored = restore_control_plane(host, args)  # never leave the stack stopped or the containers cut off
    return finish(host, res, args, stopped, restored)


def outage_checks(host, res, args, stopped):
    for label, at in (("30 s", 30), ("5 min", 300), ("end", args.outage_seconds - 20)):
        time.sleep(max(0, stopped + at - time.time()))
        for src, dst in (("a", "b"), ("b", "a")):
            ok, detail = eventually(lambda: exec_ok(host, src, dst, f"outage-{label.replace(' ', '')}-{src}{dst}"), 60, 10)
            res.add(f"outage {label}: new exec {src}->{dst}", ok, detail)
        ok, detail = eventually(lambda: terminal_ok(host, "a", "b", f"outage-{label.replace(' ', '')}-term"), 60, 10)
        res.add(f"outage {label}: new pb connect terminal a->b", ok, detail)
        rc, out = in_ct(host, NAMES["a"], f"{PB} ssh {NAMES['b']} -- echo outage-ssh", 90)
        res.add(f"outage {label}: new pb ssh a->b", rc == 0 and "outage-ssh" in out, out.strip()[-200:])
        ok, detail = refused(host, "r", "b")
        res.add(f"outage {label}: revoked r still refused", ok, detail)
        for tag in ("long-ab", "long-ba"):
            rc, out = in_ct(host, NAMES[tag[-2]], f"pgrep -f 'echo {tag}-done' >/dev/null && test ! -e /tmp/{tag}.rc && echo running", 30)
            res.add(f"outage {label}: {tag} still running", "running" in out, out.strip()[-80:] or f"rc={rc}")
        ok, detail = eventually(lambda: health_has_outage(host), 90, 10)
        res.add(f"outage {label}: pb status shows the control plane as unreachable", ok, detail)
    rc, out = in_ct(host, NAMES["a"], f"{PB} doctor", 120)
    res.add("outage: pb doctor runs and names the control plane", rc in (0, 1) and ("control" in out.lower() or "paperboat" in out.lower()), out.strip()[:200])
    time.sleep(max(0, stopped + args.outage_seconds - time.time()))


def finish(host, res, args, stopped, restored):
    rc, out = restored
    res.add("control plane reachable again", rc == 0, out.strip()[-120:])
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


def active_name(host, from_ct, stem):
    """The online enrollment named stem or stem-N: a name already used by an earlier, revoked record is numbered."""
    rc, out = in_ct(host, from_ct, f"{PB} machine list", 60)
    names = [line.split()[0] for line in out.splitlines() if re.match(rf"^{re.escape(stem)}(-\d+)?\s", line) and " online " in line]
    return names[-1] if names else stem


def revoke_machine(host, from_ct, name):
    """Revoke one exact enrollment from a signed-in machine of the account: preview, then the confirmation it prints."""
    rc, out = in_ct(host, from_ct, f"{PB} machine revoke {name} --json", 90)
    token = re.search(r'"confirmation_token":"([A-Z0-9]+)"', out)
    if not token:
        return False, out.strip()[-150:]
    rc, out = in_ct(host, from_ct, f"{PB} machine revoke {name} --json --confirm {token.group(1)}", 90)
    return rc == 0, out.strip()[-150:]


def cleanup(args):
    state = load()
    host = state["host"]
    for victim in ("r", "b", "a"):  # each from a machine that is still signed in; a last, from itself
        ok, detail = revoke_machine(host, NAMES["a"], active_name(host, NAMES["a"], NAMES[victim]))
        print(("revoked " if ok else "NOT revoked ") + NAMES[victim], detail)
    host_run(host, "sudo -n docker rm -f " + " ".join(NAMES.values()), 120)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("phase", choices=["setup", "drill", "cleanup"])
    parser.add_argument("--host", default="ubuntu@anvit")
    parser.add_argument("--binary")
    parser.add_argument("--token-cmd")
    parser.add_argument("--server-container", default="pb-release-staging-server-1")
    parser.add_argument("--outage-seconds", type=int, default=660)
    parser.add_argument("--mode", choices=["blackhole", "stop"], default="blackhole")
    parser.add_argument("--api-ip", help="the staging API address to cut off in --mode blackhole")
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
