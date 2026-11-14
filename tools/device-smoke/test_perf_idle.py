import unittest

import perf_idle

SS = """ESTAB 0 0 172.17.0.7:54586 152.67.0.60:18443 users:(("pb",pid=61,fd=9))
\t cubic bytes_sent:100 bytes_acked:90 bytes_received:50 segs_out:3
ESTAB 0 0 127.0.0.1:1 127.0.0.1:2 users:(("pb",pid=61,fd=10))
\t bytes_sent:7 bytes_received:7
ESTAB 0 0 172.17.0.7:5 1.2.3.4:443 users:(("curl",pid=9,fd=3))
\t bytes_sent:9 bytes_received:9
"""


class ParseSS(unittest.TestCase):
    def test_counts_only_pb_connections_to_other_hosts_on_any_port(self):
        self.assertEqual(perf_idle.parse_ss(SS.splitlines()), {('172.17.0.7:54586', '152.67.0.60:18443'): 150})


if __name__ == '__main__':
    unittest.main()
