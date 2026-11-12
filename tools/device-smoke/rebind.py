#!/usr/bin/env python3
"""F21-49 (N3): after a NAT rebinding, connections INTO the rebound machine work within 10 s of the network returning.

Two machines in containers on one Docker host, both paired to the same account (tools/probe/canary-setup.sh). The
machine A loses its network for --outage-seconds, and while it is away its UDP source ports change (conntrack flushed,
the host masquerades A's UDP to a different port range: a NAT rebinding). Then the network returns and B execs INTO A
until it succeeds. PASS when the first inbound exec succeeds within --limit seconds of the return and the next three
also succeed. The firewall and masquerade rules are removed on every exit path.

  rebind.py --host ubuntu@anvit --a rel-nat-a --b rel-nat-b
"""
import argparse, json, subprocess, sys, time
from pathlib import Path

HERE = Path(__file__).resolve().parent
PB = ".local/bin/pb"
RANGE = "41000-41999"


def ssh(host, script, timeout=120):
    done = subprocess.run(["ssh", "-o", "BatchMode=yes", host, script], capture_output=True, text=True, timeout=timeout)
    return done.returncode, (done.stdout + done.stderr).strip()


def in_ct(host, ct, command, timeout):
    inner = f"timeout {timeout} {command}"
    return ssh(host, f"sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 -w /home/pbt {ct} sh -c '{inner}'", timeout + 20)


def exec_into(host, src, dst, timeout=15):
    start = time.time()
    rc, out = in_ct(host, src, f"{PB} exec {dst} -- true", timeout)
    return rc == 0, time.time() - start, out[-160:]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", default="ubuntu@anvit")
    parser.add_argument("--a", required=True, help="the container that is rebound")
    parser.add_argument("--b", required=True, help="the peer that execs into it")
    parser.add_argument("--outage-seconds", type=int, default=120)
    parser.add_argument("--limit", type=float, default=10.0)
    args = parser.parse_args()
    host, checks = args.host, []

    def add(name, ok, detail):
        checks.append({"check": name, "status": "PASS" if ok else "FAIL", "detail": detail})
        print(("PASS " if ok else "FAIL ") + name + " " + detail, flush=True)

    for src, dst in ((args.a, args.b), (args.b, args.a)):
        ok, took, out = exec_into(host, src, dst, 60)
        add(f"baseline exec {src}->{dst}", ok, f"{took:.1f} s {out}")
    rc, ip = ssh(host, f"sudo -n docker inspect -f '{{{{range .NetworkSettings.Networks}}}}{{{{.IPAddress}}}}{{{{end}}}}' {args.a}")
    if rc != 0 or not ip:
        add("address of the rebound container", False, ip)
        return finish(checks)
    drop = f"sudo -n iptables -I DOCKER-USER -s {ip} -j DROP; sudo -n iptables -I DOCKER-USER -d {ip} -j DROP"
    undrop = f"sudo -n iptables -D DOCKER-USER -s {ip} -j DROP; sudo -n iptables -D DOCKER-USER -d {ip} -j DROP"
    rule = f"-s {ip} -p udp -j MASQUERADE --to-ports {RANGE} --random-fully"
    dropped = rebound = False
    try:
        rc, out = ssh(host, drop)
        dropped = True
        add("network of the rebound machine cut", rc == 0, out)
        time.sleep(min(30, args.outage_seconds // 4))
        ssh(host, f"sudo -n conntrack -D -s {ip}; sudo -n conntrack -D -d {ip}; true")
        rc, out = ssh(host, f"sudo -n iptables -t nat -I POSTROUTING {rule}")
        rebound = rc == 0
        add(f"UDP source ports of the rebound machine moved to {RANGE}", rebound, out)
        time.sleep(args.outage_seconds - min(30, args.outage_seconds // 4))
        ssh(host, undrop)
        dropped = False
        returned = time.time()
        deadline = returned + 90
        first = None
        while time.time() < deadline:
            ok, _, out = exec_into(host, args.b, args.a, 12)
            if ok:
                first = time.time() - returned
                break
        add(f"first inbound exec {args.b}->{args.a} after the network returned", first is not None and first <= args.limit,
            f"{first:.1f} s (limit {args.limit:.0f} s)" if first is not None else "no success within 90 s")
        stable = all(exec_into(host, args.b, args.a, 20)[0] for _ in range(3))
        add("the next three inbound execs succeed", stable, "")
        ok, took, out = exec_into(host, args.a, args.b, 30)
        add(f"outbound exec {args.a}->{args.b}", ok, f"{took:.1f} s {out}")
    finally:
        if dropped:
            ssh(host, undrop)
        if rebound:
            ssh(host, f"sudo -n iptables -t nat -D POSTROUTING {rule}")
    return finish(checks)


def finish(checks):
    out = HERE / "results" / f"rebind-{time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())}.json"
    out.parent.mkdir(exist_ok=True)
    out.write_text(json.dumps({"checks": checks}, indent=1) + "\n")
    print("result:", out)
    return 0 if all(c["status"] == "PASS" for c in checks) else 1


if __name__ == "__main__":
    sys.exit(main())
