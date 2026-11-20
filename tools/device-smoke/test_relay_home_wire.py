import unittest

import relay_home_wire as wire

RELAYS = [{'name': 'vantage-a', 'region': 'in'}, {'name': 'vantage-b', 'region': 'us'}, {'name': 'vantage-c', 'region': 'us'}]


class TestRelayHomeWireComparesRegions(unittest.TestCase):
    def test_relay_home_wire_compares_regions(self):
        # the status home region (what users see) maps to the relay whose address is blocked; no relay id is read
        by_region = wire.relay_regions(RELAYS, {'vantage-a': '1.1.1.1', 'vantage-b': '2.2.2.2'})
        self.assertEqual({'in': 'vantage-a', 'us': 'vantage-b'}, by_region)

    def test_two_relays_of_one_region_cannot_be_told_apart(self):
        with self.assertRaisesRegex(RuntimeError, 'different regions'):
            wire.relay_regions(RELAYS, {'vantage-b': '2.2.2.2', 'vantage-c': '3.3.3.3'})

    def test_unknown_relay_or_one_address_is_refused(self):
        with self.assertRaisesRegex(RuntimeError, 'need an address'):
            wire.relay_regions(RELAYS, {'vantage-a': '1.1.1.1'})
        with self.assertRaisesRegex(RuntimeError, 'need an address'):
            wire.relay_regions(RELAYS, {'vantage-a': '1.1.1.1', 'nope': '9.9.9.9'})


if __name__ == '__main__':
    unittest.main()
