#!/usr/bin/env python3
"""F21-160: a dead direct path is noticed in seconds, and large packets are not silently dropped.

Two machines in containers on one Docker host, paired to the same account (tools/probe/canary-setup.sh), a relay
available. Checks:
  stream     a long exec A->B prints a line every 0.5 s; mid-stream the direct UDP path between A and B is dropped
             (firewall on the host). The output must resume over the relay within --limit seconds (the dead path is
             noticed by the 3 s heartbeat, not by the 2 minute QUIC idle timeout) and every line must arrive.
  mtu        the link of A is limited to --mtu bytes (below the usual 1500); a 20 MB exec output from B must arrive
             intact (sha256 equal), because the tunnel never sends a UDP payload above the safe 1280 byte tunnel MTU.
Rules are removed on every exit path.

  direct_path.py --host ubuntu@anvit --a rel-nat-a --b rel-nat-b
"""
import argparse, base64, hashlib, json, subprocess, sys, threading, time
from pathlib import Path

HERE = Path(__file__).resolve().parent
PB = ".local/bin/pb"
LINES = 80
PERIOD = 0.5


def ssh_cmd(host, script):
    return ["ssh", "-o", "BatchMode=yes", host, script]


def ssh(host, script, timeout=120):
    done = subprocess.run(ssh_cmd(host, script), capture_output=True, text=True, timeout=timeout)
    return done.returncode, (done.stdout + done.stderr).strip()


def docker(ct):
    return f"sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 -w /home/pbt {ct}"


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", default="ubuntu@anvit")
    parser.add_argument("--a", required=True)
    parser.add_argument("--b", required=True)
    parser.add_argument("--limit", type=float, default=12.0)
    parser.add_argument("--mtu", type=int, default=1350)
    args = parser.parse_args()
    host, checks = args.host, []

    def add(name, ok, detail):
        checks.append({"check": name, "status": "PASS" if ok else "FAIL", "detail": detail})
        print(("PASS " if ok else "FAIL ") + name + " " + detail, flush=True)

    ips = {}
    for key, ct in (("a", args.a), ("b", args.b)):
        rc, ip = ssh(host, f"sudo -n docker inspect -f '{{{{range .NetworkSettings.Networks}}}}{{{{.IPAddress}}}}{{{{end}}}}' {ct}")
        ips[key] = ip
    drop = f"sudo -n iptables -I DOCKER-USER -s {ips['a']} -d {ips['b']} -j DROP; sudo -n iptables -I DOCKER-USER -s {ips['b']} -d {ips['a']} -j DROP"
    undrop = f"sudo -n iptables -D DOCKER-USER -s {ips['a']} -d {ips['b']} -j DROP; sudo -n iptables -D DOCKER-USER -s {ips['b']} -d {ips['a']} -j DROP"
    dropped = mtu_set = False
    try:
        rc, out = ssh(host, f"{docker(args.a)} {PB} exec {args.b} -- true", 90)
        add("baseline exec a->b", rc == 0, out[-120:])
        # stream: a line every PERIOD from B, the direct path dropped after 10 s
        script = f"i=0; while [ $i -lt {LINES} ]; do echo $i; i=$((i+1)); sleep {PERIOD}; done"
        proc = subprocess.Popen(ssh_cmd(host, f"{docker(args.a)} {PB} exec {args.b} -- sh -c '{script}'"), stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
        stamps = []

        def reader():
            for line in proc.stdout:
                if line.strip().isdigit():
                    stamps.append(time.time())

        thread = threading.Thread(target=reader)
        thread.start()
        time.sleep(12)
        cut = time.time()
        ssh(host, drop)
        dropped = True
        thread.join(timeout=LINES * PERIOD + 120)
        proc.wait(timeout=30)
        after = [t for t in stamps if t >= cut]
        gaps = [b - a for a, b in zip([cut] + after, after)]
        worst = max(gaps) if gaps else float("inf")
        add(f"all {LINES} lines of the stream arrived", len(stamps) == LINES, f"{len(stamps)} lines")
        add(f"stream resumed over the relay within {args.limit:.0f} s of the drop", worst <= args.limit, f"longest stall after the drop {worst:.1f} s")
        ssh(host, undrop)
        dropped = False
        time.sleep(5)
        # mtu: a smaller link on A, a large output from B
        rc, out = ssh(host, f"sudo -n docker exec {args.a} ip link set eth0 mtu {args.mtu}")
        mtu_set = rc == 0
        add(f"link of a limited to {args.mtu} bytes", mtu_set, out)
        want = hashlib.sha256()
        chunk = b"0123456789abcdef\n"
        total = 20_000_000
        want.update((chunk * (total // len(chunk) + 1))[:total])
        # the pipeline goes in as a file so no shell layer re-quotes it
        body = f"#!/bin/sh\ntimeout 250 {PB} exec {args.b} -- sh -c 'yes 0123456789abcdef | head -c {total}' 2>/dev/null | sha256sum\n"
        encoded = base64.b64encode(body.encode()).decode()
        ssh(host, f"echo {encoded} | base64 -d | sudo -n docker exec -i {args.a} sh -c 'cat > /tmp/mtu.sh'")
        rc, out = ssh(host, f"{docker(args.a)} sh /tmp/mtu.sh", 300)
        got = out.split()[0] if out.split() else ""
        add("20 MB exec output b->a intact over the limited link", got == want.hexdigest(), f"sha256 {got[:16]} want {want.hexdigest()[:16]}")
    finally:
        if dropped:
            ssh(host, undrop)
        if mtu_set:
            ssh(host, f"sudo -n docker exec {args.a} ip link set eth0 mtu 1500")
    out = HERE / "results" / f"direct-path-{time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())}.json"
    out.parent.mkdir(exist_ok=True)
    out.write_text(json.dumps({"checks": checks}, indent=1) + "\n")
    print("result:", out)
    return 0 if all(c["status"] == "PASS" for c in checks) else 1


if __name__ == "__main__":
    sys.exit(main())
