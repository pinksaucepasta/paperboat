import importlib.util
import os
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location(
    'request_mix', os.path.join(os.path.dirname(__file__), '..', 'loadtest', 'request_mix.py'))
request_mix = importlib.util.module_from_spec(spec)
spec.loader.exec_module(request_mix)

LOG = '\n'.join([
    'level=INFO msg=http_request method=POST path=/v1/machines/machine_8f9e-1/events/wait status=204 client=paperboat',
    'level=INFO msg=http_request method=POST path=/v1/machines/machine_77aa-2/events/wait status=204 client=paperboat',
    'level=INFO msg=http_request method=POST path=/v1/machine-peer-network/config status=200 client=paperboat',
    'level=INFO msg=http_request method=POST path=/v1/edge/runtime/endpoint-admissions status=200 client=Go-http-client/1.1',
    'level=INFO msg=http_request method=POST path=/v1/nodes/heartbeat status=200 client=paperboat',
    'level=INFO msg=other path=/v1/machine-services',
])


class RequestMixTest(unittest.TestCase):
    def test_counts_collapse_ids_and_skip_edge_node_and_other_lines(self):
        with mock.patch.object(request_mix.subprocess, 'run', return_value=mock.Mock(stdout=LOG)):
            got = request_mix.counts('unit', 'a', 'b')
        self.assertEqual(got, {'/v1/machines/machine_ID/events/wait': 2, '/v1/machine-peer-network/config': 1})

    def test_stream_budget_is_one_config_and_one_jwks(self):
        budget = request_mix.BUDGETS['stream_open']
        self.assertEqual(budget['/v1/peer-network/config'], ('max', 1))
        self.assertEqual(budget['/v1/jwks'], ('max', 1))


if __name__ == '__main__':
    unittest.main()
