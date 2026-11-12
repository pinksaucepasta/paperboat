#!/usr/bin/env python3
"""F21-50 (macOS, Windows): `pb doctor` local_names check on a device whose DNS is cut.

  doctor_local_names.py --mac macpool-3             a Mac runner (pb at ~/.local/bin/pb, sudo -n pfctl)
  doctor_local_names.py --windows                   a Windows runner (PB_SMOKE_WINDOWS_PB, SMOKE_WINDOWS_SSH)

1. the real resolver: local_names passes (dns-check.<local domain> resolves to the machine's gateway address)
2. DNS (port 53, UDP and TCP) dropped on the device, resolver caches flushed: local_names warns, names the domain and says what
   to do (allow it in the router or DNS filter, or a hosts entry), and machine access with pb still works (pb status)
3. the rule removed: local_names passes again
The rule is removed on every exit path; a timer on the device removes it when this script dies.
"""
import argparse, os, json, shlex, subprocess, sys, time
from pathlib import Path

HERE = Path(__file__).resolve().parent
SSH = shlex.split(os.environ.get("PB_MAC_SSH", "ssh -o BatchMode=yes"))  # PB_MAC_SSH=~/.pbmac/ssh.sh for the apple VM (host: vm)
sys.path.insert(0, str(HERE))
ANCHOR = "com.apple/pbsmoke-dns"


def mac_runner(host):
    def ssh(script, timeout=120):
        done = subprocess.run([*SSH, host, script], capture_output=True, text=True, timeout=timeout, stdin=subprocess.DEVNULL)
        return done.returncode, done.stdout + done.stderr

    def pb(*args):
        return ssh(f"$HOME/.local/bin/pb {shlex.join(args)}")

    def block():
        rules = "block drop out quick proto { tcp udp } from any to any port 53\n"
        rc, out = ssh(f"printf %s {shlex.quote(rules)} | sudo -n pfctl -a {ANCHOR} -f - && sudo -n pfctl -E 2>&1 | sed -n 's/.*Token : //p'")
        if rc:
            raise RuntimeError("pf rule not loaded: " + out[-200:])
        ssh(f"sudo -n pfctl -F states >/dev/null 2>&1; (sudo -n sh -c 'sleep 300; /sbin/pfctl -a {ANCHOR} -F all' >/dev/null 2>&1 &); dscacheutil -flushcache; sudo -n killall -HUP mDNSResponder")
        return out.strip().splitlines()[-1] if out.strip() else ""

    def unblock(token):
        ssh(f"sudo -n pfctl -a {ANCHOR} -F all; " + (f"sudo -n pfctl -X {token}" if token.isdigit() else "true"))

    return pb, block, unblock


def windows_runner():
    import winhost
    device, pbexe = winhost.device(), winhost.pb_path()
    if not device:
        raise SystemExit("PB_SMOKE_WINDOWS_PB and SMOKE_WINDOWS_SSH must name the Windows runner")
    name = "PaperboatSmoke-dns-" + str(int(time.time()))
    script = ("if($req.action -eq 'remove'){Remove-NetFirewallRule -DisplayName $req.name -ErrorAction SilentlyContinue;ipconfig /flushdns|Out-Null;exit 0}\n"
              "foreach($p in 'UDP','TCP'){[void](New-NetFirewallRule -DisplayName $req.name -Direction Outbound -Action Block -Protocol $p -RemotePort 53)}\n"
              "ipconfig /flushdns|Out-Null")

    def pb(*args):
        done = pbexe.run_remote(list(args), timeout=180)
        return done.returncode, done.stdout + done.stderr

    def block():
        device.ps("block DNS", script, action="add", name=name)
        return name

    def unblock(token):
        device.ps("unblock DNS", script, action="remove", name=token)

    return pb, block, unblock


def local_names(pb):
    rc, out = pb("doctor", "--json")
    document = json.loads(out[out.index("{"):])
    return next((c for c in document["checks"] if c["code"] == "local_names"), {})


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--mac")
    parser.add_argument("--windows", action="store_true")
    args = parser.parse_args()
    kind = "mac" if args.mac else "windows"
    pb, block, unblock = mac_runner(args.mac) if args.mac else windows_runner()
    checks = []

    def add(name, ok, detail=""):
        checks.append({"check": name, "status": "PASS" if ok else "FAIL", "detail": detail[:300]})
        print(("PASS " if ok else "FAIL ") + name + " " + detail[:300], flush=True)

    check = local_names(pb)
    add("real resolver: local_names passes", check.get("status") == "pass", check.get("summary", "no local_names check"))
    token = block()
    try:
        time.sleep(3)
        check = local_names(pb)
        text = check.get("summary", "") + " " + check.get("recovery", "")
        add("DNS dropped: local_names warns", check.get("status") == "warning", text)
        add("the warning names the domain and says what to do", "do not resolve" in text and "hosts file" in text and "router or DNS filter" in text, text)
        rc, out = pb("status")
        add("pb status still answers with DNS dropped", rc == 0, out.strip()[:120])
    finally:
        unblock(token)
    time.sleep(3)
    check = local_names(pb)
    add("DNS back: local_names passes again", check.get("status") == "pass", check.get("summary", ""))
    out = HERE / "results" / f"doctor-local-names-{kind}-{time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())}.json"
    out.parent.mkdir(exist_ok=True)
    out.write_text(json.dumps({"checks": checks}, indent=1) + "\n")
    print("result:", out)
    return 0 if all(c["status"] == "PASS" for c in checks) else 1


if __name__ == "__main__":
    sys.exit(main())
