import ast
import pathlib
import unittest

HERE = pathlib.Path(__file__).resolve().parent


def shadowed(path):
    """Names a function both calls (as the module-level function) and assigns locally: Python makes the call an UnboundLocalError."""
    tree = ast.parse(path.read_text())
    module_functions = {node.name for node in tree.body if isinstance(node, ast.FunctionDef)}
    found = []
    for function in ast.walk(tree):
        if not isinstance(function, ast.FunctionDef):
            continue
        assigned = {n.id for n in ast.walk(function) if isinstance(n, ast.Name) and isinstance(n.ctx, ast.Store)}
        called = {n.func.id for n in ast.walk(function) if isinstance(n, ast.Call) and isinstance(n.func, ast.Name)}
        found += ['%s:%s uses %s() and assigns a local %s' % (path.name, function.name, name, name)
                  for name in sorted(assigned & called & module_functions)]
    return found


class NoShadowedFunctions(unittest.TestCase):
    def test_no_check_assigns_a_local_with_the_name_of_a_module_function_it_calls(self):
        # count() in terminal_matrix.py (N3/N12/N13) and terminal() in session_share_cross.py (F3-06) each crashed whole checks this way
        found = [line for path in sorted(HERE.glob('*.py')) if not path.name.startswith('test_') for line in shadowed(path)]
        self.assertEqual([], found)


if __name__ == '__main__':
    unittest.main()
