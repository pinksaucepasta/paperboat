"""Contract of the documentation link check."""
import unittest

import check_doc_links


class DocLinksTest(unittest.TestCase):
    def check(self, files, existing):
        return check_doc_links.broken(files, lambda path: path.as_posix() in existing)

    def test_reports_missing_targets_relative_to_the_page(self):
        found = self.check({"cli/docs/cli.md": "See [guide](operations.md) and [ref](cli/pb.md#top) and [gone](missing.md)."},
                           {"cli/docs/operations.md", "cli/docs/cli/pb.md"})
        self.assertEqual(found, ["cli/docs/cli.md:1: missing.md"])

    def test_ignores_external_anchor_code_and_images_that_exist(self):
        files = {"README.md": "[a](https://example.test/x) [b](#top) [c](mailto:x@example.test)\n"
                              "`[d](nope.md)`\n```\n[e](nope.md)\n```\n![f](logo.png)\n"}
        self.assertEqual(self.check(files, {"logo.png"}), [])


if __name__ == "__main__":
    unittest.main()
