import copy
import unittest
import upgrade_matrix as matrix


class ReleaseGate(unittest.TestCase):
    def report(self):
        return {'cells': [dict(c, ok=True) for c in matrix.cells()],
                'checks': [{'step': name, 'ok': True} for name in
                           ['client_downgrade', 'held_session_rolling_server_and_edge']],
                'restoration': [{'step': name, 'ok': True} for name in
                                ['candidate_server', 'candidate_edge', 'candidate_clients']]}

    def test_all_axes_continuity_downgrade_and_cleanup_are_required(self):
        report = self.report()
        self.assertTrue(matrix.complete(report))
        for kind in ['cells', 'checks', 'restoration']:
            for index in range(len(report[kind])):
                with self.subTest(kind=kind, index=index):
                    failed = copy.deepcopy(report)
                    failed[kind][index]['ok'] = False
                    self.assertFalse(matrix.complete(failed))
            missing = copy.deepcopy(report)
            missing[kind].pop()
            self.assertFalse(matrix.complete(missing))
        duplicate = self.report()
        duplicate['cells'][0] = duplicate['cells'][1]
        self.assertFalse(matrix.complete(duplicate))

    def test_command_failure_cannot_be_a_green_cell(self):
        def failed():
            raise RuntimeError('private remote response must stay private')
        result = matrix.check('send', failed)
        self.assertFalse(result['ok'])
        self.assertEqual(result['error'], 'RuntimeError')
        self.assertNotIn('private', str(result))


class EdgeReplacement(unittest.TestCase):
    def test_failed_candidate_keeps_the_previous_edge_available(self):
        import json
        from unittest.mock import patch
        for failure in ('create', 'start'):
            with self.subTest(failure=failure):
                name = matrix.staging.EDGE_NAME
                containers = {name: {'image': 'previous', 'running': True}}
                info = {'Mounts': [{'Destination': '/state', 'Source': '/home/ubuntu/pb-release-staging/edge'}],
                        'HostConfig': {'PortBindings': {}}, 'Config': {'Cmd': ['-node-id', 'stg_node_test']}}
                def docker(args, **kwargs):
                    action = args[1]
                    if action == 'inspect':
                        return json.dumps([info])
                    if action in ('create', 'run'):
                        target = args[args.index('--name') + 1]
                        if failure == 'create' or action == 'run':
                            raise matrix.RemoteFailure(125)
                        containers[target] = {'image': 'candidate', 'running': False}
                    elif action == 'stop':
                        containers[args[-1]]['running'] = False
                    elif action == 'rename':
                        containers[args[-1]] = containers.pop(args[-2])
                    elif action == 'start':
                        if failure == 'start' and containers[args[-1]]['image'] == 'candidate':
                            raise matrix.RemoteFailure(125)
                        containers[args[-1]]['running'] = True
                    elif action == 'rm':
                        containers.pop(args[-1], None)
                    else:
                        raise AssertionError('unsupported Docker operation')
                    return ''
                with patch.object(matrix, 'command', side_effect=docker), patch.object(matrix, 'EDGE_RECIPE', None):
                    with self.assertRaises(matrix.RemoteFailure):
                        matrix.edge('paperboat-tunnel:stg-candidate')
                self.assertEqual(containers, {name: {'image': 'previous', 'running': True}})
