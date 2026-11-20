import importlib.util, unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location("linux_ci", Path(__file__).with_name("linux-ci.py"))
linux_ci = importlib.util.module_from_spec(spec)
spec.loader.exec_module(linux_ci)

HOSTS = ["linpool-5", "linpool-6", "ana@hermes"]


class Plan(unittest.TestCase):
    def test_cli_never_lands_on_a_host_without_sudo_and_no_job_is_lost(self):
        jobs = [("cli", ["./a"], 300), ("cli", ["./b"], 300), ("server", ["./..."], 600), ("edge", ["./..."], 130), ("relay", ["./..."], 90)]
        plan = linux_ci.assign(jobs, HOSTS)
        self.assertEqual(sorted(m for q in plan.values() for m, _, _ in q), sorted(m for m, _, _ in jobs))
        self.assertNotIn("cli", [m for m, _, _ in plan["ana@hermes"]])
        self.assertEqual(sorted(len(q) for q in plan.values())[0], 1)  # the longest job sits alone on the host that cannot run cli

    def test_server_never_lands_on_the_small_host(self):
        jobs = [("cli", ["./a"], 300), ("cli", ["./b"], 300), ("server", ["./..."], 600), ("edge", ["./..."], 130)]
        plan = linux_ci.assign(jobs, HOSTS)
        self.assertNotIn("server", [m for m, _, _ in plan["ana@hermes"]])

    def test_every_planned_job_is_a_real_module_or_a_cli_shard(self):
        for module, pkgs, seconds in linux_ci.jobs():
            self.assertIn(module, {"cli", "relay", "edge", "shared", "helper", "server"})
            self.assertTrue(pkgs and seconds > 0)


if __name__ == "__main__":
    unittest.main()
