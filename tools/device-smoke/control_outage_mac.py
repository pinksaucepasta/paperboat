#!/usr/bin/env python3
"""F21-01 / F21-11 (macOS): the control plane is unreachable and a Mac keeps working. Staging only, never prod.

control_outage_windows.py with a Mac as the target: MAC is the SSH host of the Mac (a macpool runner, user with
passwordless sudo, pb at ~/.local/bin/pb paired to staging as --name), L and R are containers on --host
(OUTAGE_L / OUTAGE_R, paired by tools/probe/canary-setup.sh). The cut: a pf rule on the Mac that drops TCP to the API
addresses and port (the SSH that drives the Mac goes to another port), an iptables DROP to the same in the containers;
the shared staging server is never touched. A root timer on the Mac lifts the rule whatever happens here.

  control_outage_mac.py drill  --mac macpool-3 --name relmw-mac --server URL     (as control_outage_windows.py drill)
  control_outage_mac.py revoke ...                                                (as control_outage_windows.py revoke)
"""
import argparse, os, contextlib, json, shlex, socket, subprocess, sys, time
from pathlib import Path
from urllib.parse import urlsplit

HERE = Path(__file__).resolve().parent
SSH = shlex.split(os.environ.get("PB_MAC_SSH", "ssh -o BatchMode=yes"))  # PB_MAC_SSH=~/.pbmac/ssh.sh for the apple VM (host: vm)
sys.path.insert(0, str(HERE))
import control_outage as c  # noqa: E402
import control_outage_windows as w  # noqa: E402

PB = "$HOME/.local/bin/pb"
ANCHOR = "com.apple/pbsmoke-outage"  # the stock pf.conf evaluates com.apple/* anchors, the main ruleset is not touched


def ssh(mac, script, timeout=120):
    done = subprocess.run([*SSH, mac, script], capture_output=True, text=True, timeout=timeout, stdin=subprocess.DEVNULL)
    return done.returncode, done.stdout + done.stderr


class MacProcess:
    """A detached `pb exec` on the Mac: its exit code and output are files there."""

    def __init__(self, mac, tag):
        self.mac, self.tag = mac, tag

    def returncode(self):
        rc, out = ssh(self.mac, f"cat /tmp/{self.tag}.rc 2>/dev/null", 30)
        return int(out.strip()) if out.strip().isdigit() else None

    def stdout(self):
        return ssh(self.mac, f"cat /tmp/{self.tag}.out 2>/dev/null", 30)[1].encode()

    def wait(self, timeout):
        end = time.time() + timeout
        while time.time() < end:
            code = self.returncode()
            if code is not None:
                return code
            time.sleep(5)
        raise TimeoutError("still running")


class MacCut:
    def __init__(self, env, token):
        self.env, self.token = env, token

    def release(self):
        # only this anchor's rules go: `-F all` also flushes the whole state table and kills the live peer flows
        ssh(self.env.mac, f"sudo -n pfctl -a {ANCHOR} -F rules; " + (f"sudo -n pfctl -X {self.token}" if self.token else "true"), 60)


class MacEnv:
    stem, echo, enter = "mac", "echo", "n"
    probe, answer = "$((6*7))", "42"  # typed into the terminal's shell; only that shell prints the answer

    def __init__(self, args):
        self.host, self.server, self.mac, self.name = args.host, args.server, args.mac, args.name
        self.api, self.port = urlsplit(args.server).hostname, urlsplit(args.server).port or 443
        self.ips = sorted({info[4][0] for info in socket.getaddrinfo(self.api, None, socket.AF_INET)})

    def long_command(self, seconds):
        return f"sh -c 'echo started; sleep {seconds}; echo long-lw-done'"

    @contextlib.contextmanager
    def scratch(self):
        yield "/tmp"

    def spawn_wl(self, seconds, directory):
        ssh(self.mac, "rm -f /tmp/long-wl.out /tmp/long-wl.rc", 30)
        inner = f"{PB} exec {w.L} -- sh -c 'echo started; sleep {seconds}; echo long-wl-done' > /tmp/long-wl.out 2>&1; echo $? > /tmp/long-wl.rc"
        ssh(self.mac, f"nohup sh -c {shlex.quote(inner)} < /dev/null > /dev/null 2>&1 &", 30)
        return MacProcess(self.mac, "long-wl")

    def ct(self, name, command, timeout=120):
        return c.in_ct(self.host, name, command, timeout)

    def win(self, *arguments, timeout=120):  # pb on the Mac (named win in the shared drill)
        return ssh(self.mac, f"{PB} {shlex.join(arguments)}", timeout)

    def cut(self, containers, seconds):
        rule = f"block drop out quick proto tcp from any to {{ {' '.join(self.ips)} }} port {self.port}\n"
        rc, out = ssh(self.mac, f"printf %s {shlex.quote(rule)} | sudo -n pfctl -a {ANCHOR} -f - && sudo -n pfctl -E 2>&1 | sed -n 's/.*Token : //p'", 60)
        if rc:
            raise RuntimeError("pf rule not loaded on the Mac: " + out[-200:])
        # established connections keep passing on their pf state until it is flushed; the root timer lifts the rule whatever happens here
        # an established connection to the API passes on its pf state: kill those states only, not the table
        kills = "; ".join(f"sudo -n pfctl -k 0.0.0.0/0 -k {ip}" for ip in self.ips)
        ssh(self.mac, f"{kills} >/dev/null 2>&1; (sudo -n sh -c 'sleep {seconds}; /sbin/pfctl -a {ANCHOR} -F rules' >/dev/null 2>&1 &)", 60)
        token = out.strip().splitlines()[-1] if out.strip() else ""
        return MacCut(self, token if token.isdigit() else ""), c.host_run(self.host, self.rules("-I", containers, " && "), 60)

    def rules(self, action, containers, joiner):
        return joiner.join(f"sudo -n docker exec {ct} iptables {action} OUTPUT -p tcp -d {ip} --dport {self.port} -j DROP"
                           for ct in containers for ip in self.ips)

    def restore(self, block, containers):
        block.release()
        return c.host_run(self.host, self.rules("-D", containers, "; "), 60)


def revoked_before(env, args):
    """After `revoke` R is revoked and signed out: with the control plane cut again, the Mac still refuses R and L still reaches it."""
    res = c.Result(args.redact)
    # the carrier needs a moment after the previous cut before it takes inbound again: start the cut from a working pair
    ok, detail = c.eventually(lambda: w.exec_lw(env, "prerevoked-baseline"), 240, 10)
    res.add("L reaches the Mac before the second cut", ok, detail)
    block, routes = env.cut([w.L], 300)
    try:
        res.add("Mac and L cut off from the control plane", routes[0] == 0, routes[1].strip()[-120:])
        time.sleep(20)
        ok, detail = c.eventually(lambda: w.refused_rw(env), 90, 10)
        res.add("a user revoked before the outage (R) is refused by the Mac", ok, detail)
        ok, detail = c.eventually(lambda: w.exec_lw(env, "prerevoked-LW"), 90, 10)
        res.add("a user that was not revoked (L) still reaches the Mac", ok, detail)
    finally:
        restored = env.restore(block, [w.L])
    res.add("control plane reachable again", restored[0] == 0, restored[1].strip()[-120:])
    return w.write(res, "control-outage-revoked-mac", {})


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("phase", choices=["drill", "revoke", "revoked"])
    parser.add_argument("--host", default="ubuntu@anvit")
    parser.add_argument("--server", required=True)
    parser.add_argument("--mac", required=True, help="SSH host of the Mac")
    parser.add_argument("--name", required=True, help="the Mac's machine name")
    parser.add_argument("--outage-seconds", type=int, default=660)
    parser.add_argument("--revoke-wait", type=int, default=180)
    parser.add_argument("--redact", action="append", default=[])
    args = parser.parse_args()
    w.W = args.name
    env = MacEnv(args)
    sys.exit({"drill": w.drill, "revoke": w.revoke, "revoked": revoked_before}[args.phase](env, args))


if __name__ == "__main__":
    main()
