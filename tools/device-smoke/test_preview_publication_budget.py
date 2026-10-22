"""Preview smoke waits for the bounded client and preserves publication failures."""
import subprocess
import unittest
from unittest.mock import patch

import preview_manage


class PreviewPublicationBudget(unittest.TestCase):
    def test_management_waits_for_client_publication_and_keeps_ttl_failure(self):
        leases = {}
        ended = set()
        calls = []

        def invoke(binary, *arguments, timeout=180, **kwargs):
            calls.append(arguments)
            if arguments[1] not in ('status', 'list', 'stop', 'delete'):
                # A CLI publication completing after one minute is still inside
                # its existing two-minute bound. A 60s subprocess kills it.
                if timeout < 125:
                    raise subprocess.TimeoutExpired('pb preview', timeout)
                ttl = arguments[arguments.index('--ttl') + 1]
                if ttl == '60s':
                    raise RuntimeError('user TTL expired while publication was pending')
                identity = 'prv_fixture_' + str(len(leases))
                leases[identity] = {'schema': 'paperboat.preview-tunnel/v1',
                                    'kind': 'preview_lease', 'id': identity,
                                    'state': 'ready', 'endpoint': 'https://preview.example.test'}
                return dict(leases[identity])
            operation, identity = arguments[1:3]
            if operation == 'list':
                return {'items': [dict(leases[arguments[3]])]}
            if operation in ('stop', 'delete'):
                ended.add(identity)
            result = dict(leases[identity])
            if identity in ended:
                result['state'] = 'stopped'
            return result

        with patch.object(preview_manage, 'invoke', side_effect=invoke), \
                patch.object(preview_manage, 'registration', return_value={}), \
                patch('sys.argv', ['preview_manage.py', 'fixture']):
            with self.assertRaisesRegex(RuntimeError, 'user TTL expired'):
                preview_manage.main()
        self.assertEqual(set(leases), ended)
        self.assertEqual(2, len(ended))
        self.assertTrue(any(call[1] == 'delete' for call in calls))


if __name__ == '__main__':
    unittest.main()
