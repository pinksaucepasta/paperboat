import unittest

from collect import run_all


class RunAll(unittest.TestCase):
    def test_a_failing_early_step_does_not_hide_a_later_one(self):
        ran = []

        def doctor():
            ran.append('doctor')
            raise RuntimeError('doctor did not report healthy')

        def exec_step():
            ran.append('exec')
            raise RuntimeError('exec timed out')
        with self.assertRaises(RuntimeError) as caught:
            run_all(('doctor', doctor), ('exec', exec_step), ('ok', lambda: ran.append('ok')))
        self.assertEqual(ran, ['doctor', 'exec', 'ok'])
        self.assertEqual(str(caught.exception), 'doctor: doctor did not report healthy; exec: exec timed out')

    def test_all_passing_steps_raise_nothing(self):
        run_all(('a', lambda: None), ('b', lambda: None))


if __name__ == '__main__':
    unittest.main()
