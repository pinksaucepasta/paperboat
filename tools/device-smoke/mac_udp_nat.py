#!/usr/bin/env python3
"""A NAT whose mapping changes, for one macOS machine (F21-49): pf's `nat` does not translate the host's own traffic
(checked on macOS 26), so pf routes the host's outgoing UDP to the given addresses into a utun device and this program
is the NAT behind it. Every local (address, port) flow gets its own outbound socket, bound in 41000-41999 (pf routes only UDP from other
source ports into the utun, so the NAT's own packets are not looped back); `rebind` (SIGUSR1, or --rebind-after
seconds) replaces all outbound sockets, so the far side sees the same flow arrive from a new source port while the daemon
keeps running and is told nothing. Replies are written back into the utun as if they came from the real address.
  sudo python3 mac_udp_nat.py UTUN_IP HOST_IP LOGFILE [--rebind-after N]  (prints "ready UTUNNAME" when the device is up)
Root only; run it detached, it exits after --seconds (default 600)."""
import argparse, fcntl, os, selectors, signal, socket, struct, sys, time

AF_INET = 2


def open_utun():
    ctl = socket.socket(32, socket.SOCK_DGRAM, 2)  # PF_SYSTEM, SYSPROTO_CONTROL
    info = struct.pack("I96s", 0, b"com.apple.net.utun_control")
    ident = struct.unpack("I96s", fcntl.ioctl(ctl, 0xC0644E03, info))[0]  # CTLIOCGINFO
    ctl.connect((ident, 0))  # unit 0 = next free utun
    name = ctl.getsockopt(2, 2, 256)  # SYSPROTO_CONTROL, UTUN_OPT_IFNAME
    return ctl, name.rstrip(b"\0").decode()


def outbound_socket(taken):
    """A UDP socket on a port of its own in the NAT's range, which pf leaves alone (it routes only ports outside the range)."""
    while True:
        sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        port = 41000 + int.from_bytes(os.urandom(2), "big") % 1000
        if port not in taken:
            try:
                sock.bind(("0.0.0.0", port)); taken.add(port); return sock
            except OSError:
                pass
        sock.close()


def checksum(data):
    total = sum(struct.unpack(f"!{len(data) // 2}H", data)) if len(data) % 2 == 0 else 0
    while total >> 16:
        total = (total & 0xFFFF) + (total >> 16)
    return ~total & 0xFFFF


def reply_packet(src, sport, dst, dport, payload):
    udp = struct.pack("!HHHH", sport, dport, 8 + len(payload), 0) + payload  # UDP checksum 0 = none (IPv4)
    header = struct.pack("!BBHHHBBH4s4s", 0x45, 0, 20 + len(udp), 0, 0, 64, 17, 0, socket.inet_aton(src), socket.inet_aton(dst))
    header = header[:10] + struct.pack("!H", checksum(header)) + header[12:]
    return struct.pack("!I", AF_INET) + header + udp


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("utun_ip"); parser.add_argument("host_ip"); parser.add_argument("log")
    parser.add_argument("--far", action="append", required=True, help="an address the NAT serves (repeat); packets to any other are dropped")
    parser.add_argument("--rebind-after", type=float, default=0)
    parser.add_argument("--seconds", type=int, default=600)
    args = parser.parse_args()
    tun, name = open_utun()
    os.system(f"ifconfig {name} inet {args.utun_ip} {args.utun_ip.rsplit('.', 1)[0]}.2 up")  # the far end must not be our own address
    print("ready", name, flush=True)
    flows = {}  # (local port) -> [socket, {far (addr, port)}]
    generation, taken = [0], set()
    log = open(args.log, "a", buffering=1)

    def rebind(*_):
        generation[0] += 1
        for local, (sock, far) in list(flows.items()):
            new = outbound_socket(taken)
            flows[local] = [new, far]; taken.discard(sock.getsockname()[1]); sock.close()
            log.write(f"{time.time():.3f} rebind local {local} -> outbound port {new.getsockname()[1]} (generation {generation[0]})\n")
    signal.signal(signal.SIGUSR1, rebind)
    end, rebound = time.time() + args.seconds, args.rebind_after <= 0
    start = time.time()
    while time.time() < end:
        if not rebound and time.time() - start >= args.rebind_after:
            rebind(); rebound = True
        with selectors.DefaultSelector() as selector:  # kqueue: select() refuses descriptors above 1023, which a sudo child can inherit
            for sock in [tun] + [f[0] for f in flows.values()]:
                selector.register(sock, selectors.EVENT_READ)
            ready = [key.fileobj for key, _ in selector.select(0.5)]
        for sock in ready:
            if sock is tun:
                pkt = tun.recv(65535)[4:]
                ihl = (pkt[0] & 15) * 4
                if pkt[9] != 17 or socket.inet_ntoa(pkt[16:20]) not in args.far:
                    continue
                sport, dport = struct.unpack("!HH", pkt[ihl:ihl + 4])
                dst = socket.inet_ntoa(pkt[16:20]); payload = pkt[ihl + 8:]
                if sport not in flows:
                    out = outbound_socket(taken); flows[sport] = [out, set()]
                    log.write(f"{time.time():.3f} flow local {sport} -> outbound port {out.getsockname()[1]}\n")
                flows[sport][1].add((dst, dport))
                flows[sport][0].sendto(payload, (dst, dport))
            else:
                local = next((l for l, f in flows.items() if f[0] is sock), None)
                if local is None:
                    continue
                data, (addr, port) = sock.recvfrom(65535)
                if (addr, port) in flows[local][1]:
                    tun.send(reply_packet(addr, port, args.host_ip, local, data))
    log.write("exit\n")


if __name__ == "__main__":
    main()
