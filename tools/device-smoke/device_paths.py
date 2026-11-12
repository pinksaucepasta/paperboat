#!/usr/bin/env python3
"""F21-49 (rebind), F21-51 (relay paths): the Linux checks rebind.py and relay_paths.py with a Mac or Windows machine as
the target. TARGET is a hosted runner paired to the production account (macpool / winpool), PEER a Linux container on
--host paired to the same account (tools/probe/canary-setup.sh); both end up in results/<check>-<target kind>-*.json.

  device_paths.py rebind     --mac macpool-3 --name relmw-mac  --peer relmw-p1 [--peer-ip-of ...]
  device_paths.py relay      --windows (PB_SMOKE_WINDOWS_PB, SMOKE_WINDOWS_SSH) --name relmw-win --peer relmw-p1 --relay IP --relay IP

rebind: the target loses its network for --outage-seconds (everything except the SSH that drives it), and while it is away
its UDP source ports move (macOS: the daemon restarts, pf's nat does not translate the host's own traffic; Windows: a WinDivert
filter that rewrites the source port, pydivert),
then the peer execs INTO it: the first exec must succeed within --limit s of the network returning, the next three too,
and the outbound exec from the target works.
relay: cold first contact (daemons restarted) target -> peer: relay allowed (then a 1 MB reply), every packet to the given
relays dropped, the direct path to the peer's addresses dropped.
All rules are removed on every exit path; a root/admin timer on the device removes them even when this script dies.
"""
import argparse, base64, contextlib, json, os, re, shlex, subprocess, sys, time
from pathlib import Path

HERE = Path(__file__).resolve().parent
SSH = shlex.split(os.environ.get("PB_MAC_SSH", "ssh -o BatchMode=yes"))  # PB_MAC_SSH=~/.pbmac/ssh.sh for the apple VM (host: vm)
sys.path.insert(0, str(HERE))
RANGE = "41000-41999"
PF_CUT = "com.apple/pbsmoke-cut"


def ssh(host, script, timeout=120, command=("ssh", "-o", "BatchMode=yes")):
    done = subprocess.run([*command, host, script], capture_output=True, text=True, timeout=timeout, stdin=subprocess.DEVNULL)
    return done.returncode, (done.stdout + done.stderr).strip()


def msh(host, script, timeout=120):
    return ssh(host, script, timeout, SSH)


class Mac:
    kind = "mac"
    echo = "echo"
    pb_path = "$HOME/.local/bin/pb"

    def __init__(self, host, name, cut_ips):
        self.host, self.name, self.cut_ips = host, name, cut_ips

    def pb(self, *args, timeout=120):
        return msh(self.host, f"perl -e 'alarm shift; exec @ARGV' {timeout} {self.pb_path} {shlex.join(args)}", timeout + 20)  # macOS has no timeout(1)

    def restart_daemon(self):
        return msh(self.host, f"sudo -n {self.pb_path} service restart", 120)

    def _pf(self, anchor, rules, seconds):
        rc, out = msh(self.host, f"printf %s {shlex.quote(rules)} | sudo -n pfctl -a {anchor} -f - 2>&1 && sudo -n pfctl -E 2>&1 | sed -n 's/.*Token : //p'", 60)
        if rc:
            raise RuntimeError(f"pf anchor {anchor} not loaded: {out[-200:]}")
        msh(self.host, f"sudo -n pfctl -F states >/dev/null 2>&1; (sudo -n sh -c 'sleep {seconds}; /sbin/pfctl -a {anchor} -F all' >/dev/null 2>&1 &)", 60)
        return out.splitlines()[-1] if out else ""

    def _unpf(self, anchor, token):
        msh(self.host, f"sudo -n pfctl -a {anchor} -F all; " + (f"sudo -n pfctl -X {token}" if token.isdigit() else "true"), 60)

    @contextlib.contextmanager
    def rules(self, anchor, rules, seconds):
        token = self._pf(anchor, rules, seconds)
        try:
            yield
        finally:
            self._unpf(anchor, token)

    def cut_all(self, seconds):
        """Everything pb talks to (the API and the hosted relays, which here also host the peer) is dropped, except their port 22:
        the SSH that drives the Mac may run over one of them, and a machine-wide cut would sever it."""
        ips = " ".join(self.cut_ips)
        return self.rules(PF_CUT, f"block drop quick proto {{ tcp udp }} from any to {{ {ips} }} port != 22\nblock drop quick proto {{ tcp udp }} from {{ {ips} }} port != 22 to any\n", seconds)

    def cut_addresses(self, addresses, ports, protocols, seconds):
        """Drop `protocols` traffic to and from `addresses` on `ports` (pf ranges a:b)."""
        ip, port = " ".join(addresses), " ".join(p.replace("-", ":") for p in ports)
        return self.rules(PF_CUT, f"block drop quick proto {{ {' '.join(p.lower() for p in protocols)} }} from any to {{ {ip} }} port {{ {port} }}\n"
                                  f"block drop quick proto {{ {' '.join(p.lower() for p in protocols)} }} from {{ {ip} }} port {{ {port} }} to any\n", seconds)

    def udp_ports(self):
        return set(msh(self.host, "sudo -n lsof -nP -iUDP 2>/dev/null | awk '$1==\"pb\" {print $9}' | sort -u")[1].split())

    @contextlib.contextmanager
    def rebind(self, seconds):
        """pf's nat does not translate the Mac's own traffic (its state table keeps the source port, checked on macOS 26),
        so the ports move by restarting the daemon while the network is cut: new sockets, new source ports."""
        before = self.udp_ports()
        self.restart_daemon()
        time.sleep(5)
        yield lambda: len(self.udp_ports() - before)  # daemon UDP endpoints that did not exist before


class Windows:
    kind = "windows"
    echo = "cmd /c echo"

    def __init__(self, name):
        import winhost
        self.device, self.pbexe, self.name = winhost.device(), winhost.pb_path(), name
        if not self.device:
            raise SystemExit("PB_SMOKE_WINDOWS_PB and SMOKE_WINDOWS_SSH must name the Windows runner")

    def pb(self, *args, timeout=120):
        done = self.pbexe.run_remote(list(args), timeout=timeout)
        return done.returncode, (done.stdout + done.stderr).strip()

    def restart_daemon(self):
        return self.pb("service", "restart")

    def cut_all(self, seconds):
        return self.device.cut_network(seconds)

    def cut_addresses(self, addresses, ports, protocols, seconds):
        return self.device.cut_network(seconds, addresses=addresses, ports=ports, protocols=protocols)

    @contextlib.contextmanager
    def rebind(self, seconds):
        """WinDivert (pydivert, signed driver, installed from PyPI) rewrites the source port of outgoing UDP to the
        range and the destination port of the replies back: a NAT whose mapping changed. Killed after `seconds`."""
        script = DIVERT.replace("@RANGE@", RANGE.split("-")[0]).replace("@SECONDS@", str(seconds))
        with self.device.temp_dir("pb-divert") as directory:
            path = directory / "divert.py"
            path.write_text(script)
            self.device.ps("install pydivert", "py -m pip install --quiet pydivert 2>&1 | Out-String", timeout=300)
            log = directory / "divert.log"
            log.write_text("")
            proc = self.device.spawn(["py", str(path), str(log)], directory)
            time.sleep(8)
            if proc.returncode() is not None:
                raise RuntimeError("the WinDivert NAT exited: " + proc.stderr()[-300:].decode(errors="replace"))
            try:
                yield lambda: len(log.read_text().split("\n")) - 1  # UDP flows whose source port was rewritten
            finally:
                proc.kill()
                self.device.ps("stop the WinDivert NAT", "Get-CimInstance Win32_Process | Where-Object { $_.CommandLine -like '*divert.py*' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }")


DIVERT = r'''
import pydivert, time
import sys
base, end, mapping = @RANGE@, time.time() + @SECONDS@, {}
with pydivert.WinDivert("udp and !loopback and udp.DstPort != 53 and udp.SrcPort != 53") as handle:
    for packet in handle:
        if time.time() > end:
            handle.send(packet); break
        if packet.is_outbound:
            if packet.src_port not in mapping:
                mapping[packet.src_port] = base + len(mapping) % 900
                open(sys.argv[1], "a").write(f"{packet.src_port} {mapping[packet.src_port]}\n")
            packet.src_port = mapping[packet.src_port]
        else:
            back = {v: k for k, v in mapping.items()}.get(packet.dst_port)
            if back is not None:
                packet.dst_port = back
        handle.send(packet)
'''


class Peer:
    """A Linux peer: a container ct on ssh host `host`, or (ct is the machine name, direct=True) the Linux machine at `host` itself."""

    def __init__(self, host, ct, direct=False):
        self.host, self.ct = host, ct
        self.prefix = "" if direct else f"sudo -n docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 -w /home/pbt {ct}"

    def run(self, command, timeout=60):
        return ssh(self.host, f"{self.prefix} sh -c {shlex.quote(f'timeout {timeout} {command}')}", timeout + 20)

    def exec_into(self, target, timeout=15):
        start = time.time()
        rc, out = self.run(f"$HOME/.local/bin/pb exec {target.name} -- {target.echo} peer-ok", timeout)
        return rc == 0 and "peer-ok" in out, time.time() - start, out[-160:]

    def restart_daemon(self):
        return ssh(self.host, f"{self.prefix} systemctl --user restart paperboatd")

    def addresses(self):
        """The addresses the target may reach the peer at: its public (host) address, as the relay sees it."""
        rc, out = self.run("curl -fsS --max-time 10 https://api.ipify.org || true", 20)
        return [out.strip()] if re.fullmatch(r"[0-9.]+", out.strip()) else []


class Report:
    def __init__(self, stem):
        self.stem, self.checks = stem, []

    def add(self, name, ok, detail=""):
        self.checks.append({"check": name, "status": "PASS" if ok else "FAIL", "detail": detail})
        print(("PASS " if ok else "FAIL ") + name + " " + detail, flush=True)

    def finish(self):
        out = HERE / "results" / f"{self.stem}-{time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())}.json"
        out.parent.mkdir(exist_ok=True)
        out.write_text(json.dumps({"checks": self.checks}, indent=1) + "\n")
        print("result:", out)
        return 0 if all(c["status"] == "PASS" for c in self.checks) else 1


def rebind(args, target, peer):
    report = Report(f"rebind-{target.kind}")
    ok, took, out = peer.exec_into(target, 60)
    report.add(f"baseline exec peer->{target.kind}", ok, f"{took:.1f} s {out}")
    start = time.time()
    rc, out = target.pb("exec", peer.ct, "--", "true", timeout=60)
    report.add(f"baseline exec {target.kind}->peer", rc == 0, f"{time.time() - start:.1f} s {out[-120:]}")
    total = args.outage_seconds + 60
    cut = contextlib.ExitStack()
    try:
        cut.enter_context(target.cut_all(total))
        report.add(f"network of the {target.kind} machine cut", True)
        time.sleep(min(30, args.outage_seconds // 4))
        with target.rebind(total) as moved:
            report.add(f"UDP source ports of the {target.kind} machine rebound", True)
            time.sleep(args.outage_seconds - min(30, args.outage_seconds // 4))
            cut.close()  # the network returns
            returned = time.time()
            first = None
            while time.time() < returned + 90:
                if peer.exec_into(target, 12)[0]:
                    first = time.time() - returned
                    break
            report.add(f"first inbound exec peer->{target.kind} after the network returned", first is not None and first <= args.limit,
                       f"{first:.1f} s (limit {args.limit:.0f} s)" if first is not None else "no success within 90 s")
            report.add("after the return the machine's UDP traffic uses rebound ports", moved() > 0, f"{moved()} rebound UDP ports/flows")
            report.add("the next three inbound execs succeed", all(peer.exec_into(target, 20)[0] for _ in range(3)))
            start = time.time()
            rc, out = target.pb("exec", peer.ct, "--", "true", timeout=60)
            report.add(f"outbound exec {target.kind}->peer", rc == 0, f"{time.time() - start:.1f} s {out[-120:]}")
    finally:
        cut.close()
    return report.finish()


def cold_exec(report, target, peer, label, limit):
    target.restart_daemon()
    peer.restart_daemon()
    time.sleep(10)
    start = time.time()
    rc, out = target.pb("exec", peer.ct, "--", "echo", "first-contact", timeout=120)
    took = time.time() - start
    ok = rc == 0 and "first-contact" in out
    report.add(f"{label}: cold exec {target.kind}->peer", ok and took <= limit, f"{took:.1f} s (limit {limit:.0f} s)" if ok else out[-160:])
    return ok


def relay(args, target, peer):
    """The hosted relays share their hosts with the peer here, so a relay is cut by address and relay ports, and the direct
    path to the peer by its address and every other UDP port."""
    report = Report(f"relay-paths-{target.kind}")
    cold_exec(report, target, peer, "relay allowed", args.limit)
    rc, out = target.pb("exec", peer.ct, "--", "sh", "-c", "yes xxxxxxxxxxxxxxx | head -c 1000000", timeout=120)
    report.add("relay allowed: 1 MB output arrives", rc == 0 and out.count("x") == 937500, f"{out.count('x')} of 937500 bytes")
    relay_ports = args.relay_ports.split(",")
    if args.same_lan:  # only two machines on one LAN can meet with both relays gone; behind two NATs nothing can punch through
        with target.cut_addresses(args.relay, relay_ports, ("TCP", "UDP"), 600):
            cold_exec(report, target, peer, "no usable relay (both hosted relays dropped)", args.limit)
    addresses = [args.peer_address] if args.peer_address else peer.addresses()
    report.add("address of the peer found", bool(addresses), ",".join(addresses))
    others = [f"1024-{int(relay_ports[0]) - 2}", f"{int(relay_ports[-1]) + 1}-65535"]
    with target.cut_addresses(addresses, others, ("UDP",), 600):
        cold_exec(report, target, peer, "direct path dropped, relay allowed", args.limit)
    return report.finish()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("check", choices=["rebind", "relay"])
    parser.add_argument("--host", default="ubuntu@anvit")
    parser.add_argument("--mac", help="SSH host of the Mac")
    parser.add_argument("--windows", action="store_true")
    parser.add_argument("--name", required=True, help="the target's machine name")
    parser.add_argument("--peer", required=True, help="container of the Linux peer (also its machine name)")
    parser.add_argument("--peer-ssh", help="the peer is this Linux machine itself (--peer is its machine name), not a container on --host")
    parser.add_argument("--peer-address", help="the address the target reaches the peer at (default: the peer's public address)")
    parser.add_argument("--outage-seconds", type=int, default=120)
    parser.add_argument("--limit", type=float, default=10.0)
    parser.add_argument("--cut-ip", action="append", default=[], help="Mac only, rebind: an address the cut drops (the API, the hosted relays); repeat")
    parser.add_argument("--relay", action="append", default=[], help="address of a hosted relay (repeat)")
    parser.add_argument("--same-lan", action="store_true", help="the peer is on the target's LAN: also check with no usable relay")
    parser.add_argument("--relay-ports", default="27445,27446", help="the relays' ports, first to last (TCP and UDP)")
    args = parser.parse_args()
    target = Mac(args.mac, args.name, args.cut_ip) if args.mac else Windows(args.name)
    peer = Peer(args.peer_ssh or args.host, args.peer, direct=bool(args.peer_ssh))
    if args.check == "relay":
        args.limit = 15.0
    sys.exit(rebind(args, target, peer) if args.check == "rebind" else relay(args, target, peer))


if __name__ == "__main__":
    main()
