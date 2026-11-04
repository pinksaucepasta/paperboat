import os, tempfile, unittest
import check_duplicate_packages as c


class T(unittest.TestCase):
    def test_flags_identical_across_modules_only(self):
        with tempfile.TemporaryDirectory() as r:
            for d, body in (("relay/internal/x", "package x\n"), ("helper/internal/x", "package x\n"),
                            ("cli/internal/y", "package y\n"), ("cli/internal/z", "package y\n")):
                os.makedirs(os.path.join(r, d))
                open(os.path.join(r, d, "a.go"), "w").write(body)
            self.assertEqual(c.duplicates(r), [("relay/internal/x", "helper/internal/x")])


if __name__ == "__main__":
    unittest.main()
