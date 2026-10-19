"""Contract of the documented-environment-variable check."""
import unittest

import check_doc_envvars


class DocEnvVarsTest(unittest.TestCase):
    def test_reports_variables_no_code_mentions(self):
        found = check_doc_envvars.undocumented({"server/README.md": "Set PAPERBOAT_REAL and PAPERBOAT_GONE."}, "x := os.Getenv(\"PAPERBOAT_REAL\")")
        self.assertEqual(found, ["server/README.md: PAPERBOAT_GONE"])

    def test_file_variants_follow_their_base_name(self):
        self.assertEqual(check_doc_envvars.undocumented({"a.md": "PAPERBOAT_KEY_FILE"}, "lookup(name + \"_FILE\") // PAPERBOAT_KEY"), [])
        self.assertEqual(check_doc_envvars.undocumented({"a.md": "PAPERBOAT_KEY_FILE"}, "nothing"), ["a.md: PAPERBOAT_KEY_FILE"])


if __name__ == "__main__":
    unittest.main()
