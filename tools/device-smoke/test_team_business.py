"""The audit-stream smoke check must prove delivery from its own run."""
import contextlib
import gzip
import io
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import team_business


class InertStreamControl:
    """Accept setup requests without delivering events; the object store is real files."""

    windows = False
    step = team_business.Check.step

    def __init__(self, root):
        self.a = SimpleNamespace(s3_origin='127.0.0.1:7070', s3_root=str(root),
                                 s3_bucket='audit', s3_region='test-region',
                                 s3_list_cmd='sh -c')
        self.fails = []

    def run(self, *args, **kwargs):
        return 0, '', ''

    def api(self, *args, **kwargs):
        return {'status': 200}


class S3StreamFreshness(unittest.TestCase):
    def test_prior_run_objects_do_not_prove_new_delivery(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            old = root / 'audit/f19/team-team_fixture/2026/11/10/old.ndjson.gz'
            old.parent.mkdir(parents=True)
            with gzip.open(old, 'wt') as output:
                output.write('{"action":"team.role"}\n')
            control = InertStreamControl(root)
            printed = io.StringIO()
            with patch.object(team_business, 'tunnel', return_value=(
                    'https://store.example.test', 'tunnel_fixture')), \
                    patch.object(team_business, 'touch_role'), \
                    patch.object(team_business.time, 'sleep'), \
                    patch.object(team_business, 'wait', side_effect=lambda probe, *args: probe()), \
                    contextlib.redirect_stdout(printed):
                team_business.stream_s3(control, 'team_fixture', 'member_fixture')

            failures = [line for line in printed.getvalue().splitlines()
                        if line.startswith('FAIL ')]
            self.assertEqual(1, len(failures), printed.getvalue())
            self.assertTrue(failures[0].startswith('FAIL events land as gzip NDJSON'),
                            printed.getvalue())


if __name__ == '__main__':
    unittest.main()
