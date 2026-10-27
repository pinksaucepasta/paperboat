import io
import json
import unittest
from contextlib import redirect_stdout
from unittest import mock

import final_fixtures


class FinalFixturesTest(unittest.TestCase):
    def test_team_name_is_stable_per_account_and_case_insensitive(self):
        a = final_fixtures.team_name("Someone@Example.test")
        self.assertEqual(a, final_fixtures.team_name("someone@example.test"))
        self.assertNotEqual(a, final_fixtures.team_name("other@example.test"))
        self.assertRegex(a, r"^paperboat-smoke-[0-9a-f]{12}$")

    def test_refuses_anything_that_is_not_an_email(self):
        for bad in ("x", "a b@example.test", "a@b@c", "'; drop table users;--@example.test"):
            with self.assertRaises(SystemExit):
                final_fixtures.main(["--email", bad])

    def test_create_prints_one_json_line_with_the_team(self):
        calls = []
        def fake(sql, variables):
            calls.append(variables)
            return "user_x\n" if "FROM paperboat.users" in sql and "gset" not in sql else ""
        out = io.StringIO()
        with mock.patch.object(final_fixtures, "run_sql", fake), redirect_stdout(out):
            self.assertEqual(final_fixtures.main(["--email", "a@example.test", "--member", "b@example.test", "--wait", "1"]), 0)
        team = final_fixtures.team_name("a@example.test")
        self.assertEqual(json.loads(out.getvalue()), {"env": {"PAPERBOAT_SMOKE_TEAM": team}})
        self.assertIn({"email": "a@example.test", "team": team}, calls)
        self.assertIn({"team": team, "member": "b@example.test"}, calls)

    def test_waits_for_the_first_sign_in_then_gives_up_with_a_clear_message(self):
        with mock.patch.object(final_fixtures, "run_sql", lambda sql, variables: ""), mock.patch.object(final_fixtures.time, "sleep", lambda s: None):
            with self.assertRaises(SystemExit) as raised:
                final_fixtures.main(["--email", "a@example.test", "--wait", "0"])
        self.assertIn("no signed-in account yet", str(raised.exception))

    def test_teardown_reports_the_remaining_active_members(self):
        out = io.StringIO()
        with mock.patch.object(final_fixtures, "run_sql", lambda sql, variables: "1\n"), redirect_stdout(out):
            self.assertEqual(final_fixtures.main(["--email", "a@example.test", "--teardown"]), 0)
        self.assertEqual(json.loads(out.getvalue())["teardown"]["active_members"], 1)

    def test_psql_override_is_used_for_local_runs(self):
        with mock.patch.dict(final_fixtures.os.environ, {"FINAL_FIXTURES_PSQL": "docker exec -i c psql -U postgres -d dst"}):
            cmd = final_fixtures.psql_command({"team": "t"})
        self.assertEqual(cmd[:7], ["docker", "exec", "-i", "c", "psql", "-U", "postgres"])
        self.assertEqual(cmd[-2:], ["-v", "team=t"])


if __name__ == "__main__":
    unittest.main()
