import copy
import hashlib
import importlib.util
import json
import pathlib
import tempfile
import unittest


SPEC = importlib.util.spec_from_file_location(
    "license_exceptions", pathlib.Path(__file__).with_name("verify-license-exceptions.py")
)
VERIFIER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(VERIFIER)


class ReviewedLicensesTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = pathlib.Path(temporary.name)
        self.recorded = self.root / "recorded"
        self.downloaded = self.root / "downloaded"
        self.recorded.mkdir()
        self.downloaded.mkdir()
        license_text = b"independently reviewed license bytes\n"
        (self.recorded / "license.txt").write_bytes(license_text)
        (self.downloaded / "LICENSE").write_bytes(license_text)
        self.registry = {
            "example.test/library@v1.2.3": {
                "spdx": "MIT-0",
                "license_file": "license.txt",
                "module_license_file": "LICENSE",
                "sha256": hashlib.sha256(license_text).hexdigest(),
            }
        }
        self.modules = {
            "example.test/library": {"Version": "v1.2.3", "Dir": str(self.downloaded)}
        }

    def verify(self, registry=None, modules=None):
        return VERIFIER.reviewed_ignores(
            self.registry if registry is None else registry,
            self.modules if modules is None else modules,
            self.recorded,
        )

    def test_only_exact_reviewed_version_is_excluded(self):
        self.assertEqual(self.verify(), ["example.test/library"])
        bumped = copy.deepcopy(self.modules)
        bumped["example.test/library"]["Version"] = "v1.2.4"
        with self.assertRaisesRegex(ValueError, "requires license review"):
            self.verify(modules=bumped)
        self.assertEqual(self.verify(modules={}), [])

    def test_changed_downloaded_or_recorded_text_is_refused(self):
        for path in [self.recorded / "license.txt", self.downloaded / "LICENSE"]:
            with self.subTest(path=path.name):
                original = path.read_bytes()
                path.write_bytes(b"different, unreviewed terms\n")
                with self.assertRaisesRegex(ValueError, "license differs"):
                    self.verify()
                path.write_bytes(original)

    def test_replacement_or_prefix_collision_cannot_bypass_classification(self):
        for addition in ["replacement", "prefix collision"]:
            with self.subTest(addition=addition):
                modules = copy.deepcopy(self.modules)
                if addition == "replacement":
                    modules["example.test/library"]["Replace"] = {"Dir": "unreviewed"}
                else:
                    modules["example.test/library-extra"] = {"Version": "v1.0.0"}
                with self.assertRaises(ValueError):
                    self.verify(modules=modules)

    def test_recorded_paths_cannot_escape_the_license_directory(self):
        registry = copy.deepcopy(self.registry)
        registry["example.test/library@v1.2.3"]["license_file"] = "../downloaded/LICENSE"
        with self.assertRaisesRegex(ValueError, "must stay inside"):
            self.verify(registry=registry)

    def test_go_workspace_json_stream_keeps_selected_versions(self):
        data = '\n'.join(json.dumps({"Path": name, **metadata}) for name, metadata in self.modules.items())
        data += '\n' + json.dumps({"Path": "example.test/other", "Version": "v2.0.0"}) + '\n'
        selected = VERIFIER.module_records(data)
        self.assertEqual(self.verify(modules=selected), ["example.test/library"])


if __name__ == "__main__":
    unittest.main()
