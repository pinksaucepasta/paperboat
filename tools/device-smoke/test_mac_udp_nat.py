import socket, struct, sys, unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import mac_udp_nat


class ReplyPacketTest(unittest.TestCase):
    def test_a_reply_is_a_valid_ipv4_udp_packet_from_the_far_address(self):
        packet = mac_udp_nat.reply_packet("203.0.113.7", 7275, "192.0.2.2", 49519, b"hello")
        self.assertEqual(struct.unpack("!I", packet[:4])[0], mac_udp_nat.AF_INET)
        ip, udp = packet[4:24], packet[24:]
        self.assertEqual(mac_udp_nat.checksum(ip), 0)  # a header with its own checksum sums to zero
        self.assertEqual(socket.inet_ntoa(ip[12:16]), "203.0.113.7")
        self.assertEqual(socket.inet_ntoa(ip[16:20]), "192.0.2.2")
        self.assertEqual(struct.unpack("!HHH", udp[:6]), (7275, 49519, 13))
        self.assertEqual(udp[8:], b"hello")

    def test_outbound_sockets_stay_in_the_nat_range_and_never_repeat(self):
        taken, socks = set(), []
        try:
            for _ in range(20):
                socks.append(mac_udp_nat.outbound_socket(taken))
            ports = [s.getsockname()[1] for s in socks]
            self.assertEqual(len(set(ports)), 20)
            self.assertTrue(all(41000 <= p <= 41999 for p in ports))
        finally:
            for s in socks:
                s.close()


if __name__ == "__main__":
    unittest.main()
