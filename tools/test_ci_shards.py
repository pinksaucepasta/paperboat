import importlib.util, unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location("ci_shards", Path(__file__).with_name("ci-shards.py"))
shards = importlib.util.module_from_spec(spec)
spec.loader.exec_module(shards)


class Split(unittest.TestCase):
    def test_every_package_lands_in_exactly_one_shard_and_the_load_is_balanced(self):
        weight = {f"./p{i}": w for i, w in enumerate([300, 200, 120, 100, 90, 80, 60, 50, 40, 30, 20, 10])}
        result = shards.split(weight, 5)
        placed = sorted(p for _, pkgs in result for p in pkgs)
        self.assertEqual(placed, sorted(weight))
        seconds = [s for s, _ in result]
        self.assertEqual(sum(seconds), sum(weight.values()))
        self.assertLessEqual(max(seconds), max(weight.values()))  # no shard is worse than the biggest package alone

    def test_a_package_without_a_recorded_weight_uses_its_test_file_count(self):
        self.assertEqual(shards.weigh({"./a": 3, "./b": 1}, {"./b": 99}), {"./a": 11, "./b": 99})

    def test_fewer_packages_than_shards_make_fewer_shards(self):
        self.assertEqual(len(shards.split({"./a": 1, "./b": 2}, 5)), 2)


if __name__ == "__main__":
    unittest.main()
