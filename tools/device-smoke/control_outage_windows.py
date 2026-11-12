#!/usr/bin/env python3
"""F21-01 (Windows): the control plane is unreachable and a Windows machine keeps working. Staging only, never prod.

Machines, all paired to the staging server (--server): win-stg-w is the Windows device (SMOKE_WINDOWS_SSH and
PB_SMOKE_WINDOWS_PB), win-stg-l and win-stg-r are containers on --host (tools/probe/canary-setup.sh). The cut is the one
control_outage.py makes: the Windows machine's pb.exe gets a firewall block to the API addresses (the SSH that drives it is
another program), the containers an iptables DROP to the API address and port; only the API port is cut because the staging relay
(the path between the machines) answers on the same address; the shared staging server is never touched.

  control_outage_windows.py drill  [--outage-seconds 660]   long execs both ways, cut, checks at 30 s / 5 min / end, restore, recovery
  control_outage_windows.py revoke                          L revokes R while W and R are cut off; R still reaches W; after the
                                                            connection returns R is refused (host-side removal on Windows)
"""
import argparse, base64, json, socket, sys, time
from pathlib import Path
from urllib.parse import urlsplit

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import control_outage as c  # noqa: E402
import winhost  # noqa: E402

import os  # noqa: E402
L, R = os.environ.get("OUTAGE_L", "win-stg-l"), os.environ.get("OUTAGE_R", "win-stg-r")
W = os.environ.get("OUTAGE_WINDOWS_NAME", "win-stg-w")  # the target machine; control_outage_mac sets it to the Mac
PB = c.PB


class Env:
    def __init__(self, args):
        self.host, self.server = args.host, args.server
        self.device, self.pb = winhost.device(), winhost.pb_path()
        if not self.device:
            raise SystemExit("PB_SMOKE_WINDOWS_PB (and SMOKE_WINDOWS_SSH) must name the Windows device")
        self.api, self.port = urlsplit(args.server).hostname, urlsplit(args.server).port or 443
        self.ips = sorted({info[4][0] for info in socket.getaddrinfo(self.api, None, socket.AF_INET)})

    stem, echo, enter = "windows", "cmd /c echo", "r"  # what runs and presses Enter on this target (a Mac overrides them)
    probe, answer = "%%OS%%", "Windows_NT"  # typed into the terminal's shell (cmd.exe: %OS%, doubled for printf) and what only that shell prints

    def long_command(self, seconds):
        return f"cmd /c 'ping -n {seconds} 127.0.0.1 >nul & echo long-lw-done'"

    def scratch(self):
        return self.device.temp_dir("pb-outage")

    def spawn_wl(self, seconds, directory):
        return self.device.spawn([str(self.pb), "exec", L, "--", "sh", "-c", f"sleep {seconds}; echo long-wl-done"], directory)

    def ct(self, name, command, timeout=120):
        return c.in_ct(self.host, name, command, timeout)

    def win(self, *arguments, timeout=120):
        # --server: the Windows CLI is signed in to the staging server only through its own flag
        done = self.pb.run_remote(["--server", self.server, *arguments], timeout=timeout)
        return done.returncode, done.stdout + done.stderr

    def cut(self, containers, seconds):
        """Firewall block on the Windows pb.exe (lifted by a timer on the device whatever happens here) and an iptables
        DROP in each container, both for the API address and port only; returns what restore() needs."""
        block = self.device.cut_network(seconds, addresses=self.ips, ports=[self.port])
        return block, c.host_run(self.host, self.rules("-I", containers, " && "), 60)

    def rules(self, action, containers, joiner):
        return joiner.join(f"sudo -n docker exec {ct} iptables {action} OUTPUT -p tcp -d {ip} --dport {self.port} -j DROP"
                           for ct in containers for ip in self.ips)

    def restore(self, block, containers):
        block.release()
        return c.host_run(self.host, self.rules("-D", containers, "; "), 60)


def exec_lw(env, marker):
    rc, out = env.ct(L, f"{PB} exec {W} -- {env.echo} {marker}", 90)
    return rc == 0 and marker in out, out.strip()[-200:]


def exec_wl(env, marker):
    rc, out = env.win("exec", L, "--", "echo", marker, timeout=90)
    return rc == 0 and marker in out, out.strip()[-200:]


def exec_rw(env, marker):
    rc, out = env.ct(R, f"{PB} exec {W} -- {env.echo} {marker}", 90)
    return rc == 0 and marker in out, out.strip()[-200:]


def refused_rw(env):
    rc, out = env.ct(R, f"{PB} exec {W} -- {env.echo} should-not-run", 90)
    return rc != 0 and "should-not-run" not in out, out.strip()[-200:]


def terminal_lw(env, marker):
    """A new `pb connect` to the Windows machine: type a command and look for its result (the terminal shell is cmd.exe:
    the typed text shows %OS%, only the shell on the machine prints Windows_NT)."""
    script = f"""#!/bin/bash
(sleep 15; printf 'echo {marker}-{env.probe}\\{env.enter}'; sleep 4; printf 'exit\\{env.enter}'; sleep 4) | timeout 90 script -qec '{PB} connect {W}' /dev/null 2>&1 | tr -d '\\r'
"""
    encoded = base64.b64encode(script.encode()).decode()
    env.ct(L, f"echo {encoded} | base64 -d > /tmp/term-{marker}.sh", 30)
    rc, out = env.ct(L, f"bash /tmp/term-{marker}.sh", 110)
    return f"{marker}-{env.answer}" in out, out.strip()[-160:]


def status_unavailable(env):
    rc, out = env.win("status", timeout=90)
    return "Control plane is unavailable" in out, out.strip()[:300]


def status_healthy(env):
    rc, out = env.win("status", timeout=90)
    return rc == 0 and "Control plane is unavailable" not in out and "Daemon: ready" in out, out.strip()[:300]


def start_long(env, seconds, directory):
    """One long exec each way, started before the cut: L -> W in the container, W -> L as a detached process."""
    c.detached(env.host, L, f"rm -f /tmp/long-lw.out /tmp/long-lw.rc; {PB} exec {W} -- {env.long_command(seconds)} < /dev/null > /tmp/long-lw.out 2>&1; echo $? > /tmp/long-lw.rc")
    return env.spawn_wl(seconds, directory)


def long_running(env, wl):
    rc, out = env.ct(L, "test ! -e /tmp/long-lw.rc && pgrep -f long-lw-done >/dev/null && echo running", 30)
    return {"long L->W": "running" in out, "long W->L": wl.returncode() is None}


def drill(env, args):
    res = c.Result(args.redact)
    for label, fn in (("L->W", exec_lw), ("W->L", exec_wl)):
        ok, detail = c.eventually(lambda: fn(env, "baseline-" + label[0] + label[-1]), 180)
        res.add(f"baseline exec {label}", ok, detail)
    with env.scratch() as directory:
        wl = start_long(env, args.outage_seconds + 120, directory)
        time.sleep(15)
        stopped = time.time()
        block, routes = env.cut([L], args.outage_seconds + 300)
        res.add("control plane unreachable for W (firewall) and L (iptables)", routes[0] == 0, routes[1].strip()[-120:])
        restored = None
        try:
            ok, detail = c.eventually(lambda: status_unavailable(env), 120, 10)
            res.add("W: pb status shows the control plane as unreachable", ok, detail)
            for label, at in (("30 s", 30), ("5 min", 300), ("end", args.outage_seconds - 20)):
                time.sleep(max(0, stopped + at - time.time()))
                tag = label.replace(" ", "")
                for name, fn in (("L->W", exec_lw), ("W->L", exec_wl)):
                    ok, detail = c.eventually(lambda: fn(env, f"outage-{tag}-{name[0]}{name[-1]}"), 60, 10)
                    res.add(f"outage {label}: new exec {name}", ok, detail)
                ok, detail = c.eventually(lambda: terminal_lw(env, f"outage-{tag}-term"), 60, 10)
                res.add(f"outage {label}: new pb connect terminal L->W", ok, detail)
                rc, out = env.ct(L, f"{PB} ssh {W} -- echo outage-ssh", 90)
                res.add(f"outage {label}: new pb ssh L->W", rc == 0 and "outage-ssh" in out, out.strip()[-200:])
                for name, ok in long_running(env, wl).items():
                    res.add(f"outage {label}: {name} still running", ok)
                ok, detail = status_unavailable(env)
                res.add(f"outage {label}: W pb status still shows the control plane as unreachable", ok, detail)
            rc, out = env.win("doctor", timeout=180)
            res.add("outage: W pb doctor runs and names the control plane", rc in (0, 1) and ("control" in out.lower() or "paperboat" in out.lower()), out.strip()[:200])
            time.sleep(max(0, stopped + args.outage_seconds - time.time()))
        finally:
            restored = env.restore(block, [L])  # never leave the machines cut off
        res.add("control plane reachable again", restored[0] == 0, restored[1].strip()[-120:])
        began = time.time()
        ok, detail = c.eventually(lambda: status_healthy(env), 180, 5)
        res.add("recovery: W pb status healthy again", ok, f"{int(time.time() - began)} s after the start; {detail}")
        for name, fn in (("L->W", exec_lw), ("W->L", exec_wl)):
            ok, detail = c.eventually(lambda: fn(env, f"recovered-{name[0]}{name[-1]}"), 120)
            res.add(f"recovery: new exec {name}", ok, detail)
        ok, detail = c.eventually(lambda: (lambda o: ("long-lw-done" in o[1] and "0" in env.ct(L, "cat /tmp/long-lw.rc", 30)[1], o[1].strip()[-120:]))(env.ct(L, "cat /tmp/long-lw.out", 30)), 240, 10)
        res.add("recovery: long L->W finished rc 0 with its output", ok, detail)
        try:
            code = wl.wait(240)
        except Exception as error:  # noqa: BLE001 - the result names the step
            code = f"not finished: {error}"
        res.add("recovery: long W->L finished rc 0 with its output", code == 0 and b"long-wl-done" in wl.stdout(), f"rc={code} out={wl.stdout()[-60:]!r}")
    return write(res, f"control-outage-{env.stem}", {"outage_seconds": args.outage_seconds})


def revoke(env, args):
    res = c.Result(args.redact)
    ok, detail = c.eventually(lambda: exec_rw(env, "revoke-baseline"), 420, 15)
    res.add("baseline exec R->W before the revocation", ok, detail)
    block, routes = env.cut([R], 600)
    res.add("W and R cut off from the control plane", routes[0] == 0, routes[1].strip()[-120:])
    restored = None
    try:
        time.sleep(30)
        ok, detail = c.revoke_machine(env.host, L, c.active_name(env.host, L, R))
        res.add("L revokes R during the outage", ok, detail)
        time.sleep(30)
        ok, detail = exec_rw(env, "revoke-in-outage")
        res.add("during the outage R still reaches W from W's rules (the revocation has not arrived)", ok, detail)
    finally:
        restored = env.restore(block, [R])
    res.add("control plane reachable again", restored[0] == 0, restored[1].strip()[-120:])
    began = time.time()
    ok, detail = c.eventually(lambda: refused_rw(env), args.revoke_wait, 10)
    res.add(f"after the connection returned R is refused within {args.revoke_wait} s", ok, f"{int(time.time() - began)} s; {detail}")
    return write(res, f"control-outage-revoke-{env.stem}", {})


def write(res, stem, extra):
    path = HERE / "results" / f"{stem}-{time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())}.json"
    path.parent.mkdir(exist_ok=True)
    path.write_text(json.dumps({**extra, "checks": res.checks}, indent=1) + "\n")
    print("result:", path)
    for check in res.checks:
        print(check["status"], check["check"], "-", check["detail"][:120])
    return 0 if all(check["status"] == "PASS" for check in res.checks) else 1


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("phase", choices=["drill", "revoke"])
    parser.add_argument("--host", default="ubuntu@anvit")
    parser.add_argument("--server", required=True, help="the staging API base URL the machines are paired to")
    parser.add_argument("--outage-seconds", type=int, default=660)
    parser.add_argument("--revoke-wait", type=int, default=180)
    parser.add_argument("--redact", action="append", default=[], help="host names to replace with example.test in the result")
    args = parser.parse_args()
    env = Env(args)
    sys.exit(drill(env, args) if args.phase == "drill" else revoke(env, args))


if __name__ == "__main__":
    main()
