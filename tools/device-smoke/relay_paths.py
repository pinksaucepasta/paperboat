#!/usr/bin/env python3
"""F21-51 (N5): two machines reach each other with no usable relay, and over the relay while direct is unproven.

Two machines in containers on one Docker host (one LAN), paired to the same account (tools/probe/canary-setup.sh). The
client daemon (paperboatd) is restarted before each case so the first exec is a cold first contact; the time of that
exec is recorded as the time to first packet. Cases:
  relay allowed             baseline: cold exec A->B, then a 1 MB send by exec output
  relay blocked             every packet to the relays' addresses dropped for both machines: cold exec A->B works over the LAN
  direct blocked, relay on  the direct path between A and B dropped (the signed endpoints are unreachable): cold exec works over the relay
Rules are removed on every exit path.

  relay_paths.py --host ubuntu@anvit --a rel-nat-a --b rel-nat-b --relay 203.0.113.10 --relay 203.0.113.11
"""
import argparse, base64, json, subprocess, sys, time
from pathlib import Path

HERE = Path(__file__).resolve().parent
PB = ".local/bin/pb"


def ssh(host, script, timeout=120):
    done = subprocess.run(["ssh", "-o", "BatchMode=yes", host, script], capture_output=True, text=True, timeout=timeout)
    return done.returncode, (done.stdout + done.stderr).strip()


def docker(ct):
    return f"sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 -w /home/pbt {ct}"


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", default="ubuntu@anvit")
    parser.add_argument("--a", required=True)
    parser.add_argument("--b", required=True)
    parser.add_argument("--relay", action="append", required=True, help="a relay address to block (repeat)")
    parser.add_argument("--limit", type=float, default=15.0)
    args = parser.parse_args()
    host, checks = args.host, []

    def add(name, ok, detail):
        checks.append({"check": name, "status": "PASS" if ok else "FAIL", "detail": detail})
        print(("PASS " if ok else "FAIL ") + name + " " + detail, flush=True)

    ips = {k: ssh(host, f"sudo -n docker inspect -f '{{{{range .NetworkSettings.Networks}}}}{{{{.IPAddress}}}}{{{{end}}}}' {ct}")[1] for k, ct in (("a", args.a), ("b", args.b))}
    rules = []

    def block(rule):
        rules.append(rule)
        ssh(host, f"sudo -n iptables -I DOCKER-USER {rule}")

    def unblock_all():
        while rules:
            ssh(host, f"sudo -n iptables -D DOCKER-USER {rules.pop()}")

    def cold_exec(label):
        ssh(host, f"{docker(args.a)} systemctl --user restart paperboatd; {docker(args.b)} systemctl --user restart paperboatd")
        time.sleep(8)
        start = time.time()
        rc, out = ssh(host, f"{docker(args.a)} timeout 120 {PB} exec {args.b} -- sh -c 'echo first-contact'", 150)
        took = time.time() - start
        ok = rc == 0 and "first-contact" in out
        add(f"{label}: cold exec a->b", ok and took <= args.limit, f"{took:.1f} s (limit {args.limit:.0f} s)" if ok else out[-160:])
        return ok

    try:
        cold_exec("relay allowed")
        body = f"#!/bin/sh\ntimeout 120 {PB} exec {args.b} -- sh -c 'yes xxxxxxxxxxxxxxx | head -c 1000000' 2>/dev/null | wc -c\n"
        ssh(host, f"echo {base64.b64encode(body.encode()).decode()} | base64 -d | sudo -n docker exec -i {args.a} sh -c 'cat > /tmp/send.sh'")
        rc, out = ssh(host, f"{docker(args.a)} sh /tmp/send.sh", 150)
        add("relay allowed: 1 MB output arrives", out.strip().split()[-1:] == ["1000000"], out.strip()[-60:])
        for relay in args.relay:
            for ip in ips.values():
                block(f"-s {ip} -d {relay} -j DROP")
        cold_exec("relay blocked (same LAN)")
        unblock_all()
        for src, dst in ((ips["a"], ips["b"]), (ips["b"], ips["a"])):
            block(f"-s {src} -d {dst} -j DROP")
        cold_exec("direct path blocked, relay allowed")
    finally:
        unblock_all()
    out = HERE / "results" / f"relay-paths-{time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())}.json"
    out.parent.mkdir(exist_ok=True)
    out.write_text(json.dumps({"checks": checks}, indent=1) + "\n")
    print("result:", out)
    return 0 if all(c["status"] == "PASS" for c in checks) else 1


if __name__ == "__main__":
    sys.exit(main())
