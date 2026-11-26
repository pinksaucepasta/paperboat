"""Guard the public image action inputs consumed by the runtime build."""
from pathlib import Path
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]


class ServiceImageContractTest(unittest.TestCase):
    def test_each_image_receives_its_reporting_identity_and_source_revision(self):
        workflow = yaml.safe_load((ROOT / ".github/workflows/server-container.yml").read_text())
        job = workflow["jobs"]["publish"]
        matrix = job["strategy"]["matrix"]["include"]
        expected = {"server": "SENTRY_DSN_SERVER", "relay": "SENTRY_DSN_RELAY",
                    "edge": "SENTRY_DSN_EDGE"}
        self.assertEqual({row["service"]: row.get("sentry_variable") for row in matrix}, expected)
        build = next(step for step in job["steps"]
                     if step.get("uses", "").startswith("docker/build-push-action@"))
        self.assertEqual(build["with"]["tags"],
                         "ghcr.io/${{ github.repository }}/${{ matrix.service }}:${{ inputs.release_tag }}")
        arguments = dict(line.split("=", 1) for line in build["with"]["build-args"].splitlines())
        self.assertEqual(arguments["PAPERBOAT_SENTRY_DSN"], "${{ vars[matrix.sentry_variable] }}")
        self.assertEqual(arguments["PAPERBOAT_VERSION"], "${{ inputs.release_tag }}")
        self.assertIn("org.opencontainers.image.revision=${{ steps.source.outputs.sha }}",
                      build["with"]["labels"].splitlines())
        source = next(step for step in job["steps"] if step.get("id") == "source")
        self.assertIn('test "$(git rev-parse HEAD)" = "$source_sha"', source["run"])
        self.assertIn('printf \'sha=%s\\n\' "$source_sha" >> "$GITHUB_OUTPUT"', source["run"])

    def test_every_release_publishes_images_then_selfhost_packages(self):
        release = yaml.safe_load((ROOT / ".github/workflows/release.yml").read_text())
        images = release["jobs"]["service-containers"]
        selfhost = release["jobs"]["selfhost-release"]
        self.assertEqual(images["uses"], "./.github/workflows/server-container.yml")
        self.assertEqual(images["needs"], "release-publication")
        self.assertEqual(images["permissions"]["packages"], "write")
        self.assertEqual(selfhost["uses"], "./.github/workflows/selfhost-release.yml")
        self.assertEqual(selfhost["needs"], "service-containers")
        self.assertEqual(selfhost["permissions"]["contents"], "write")
        for job in (images, selfhost):
            self.assertEqual(job["secrets"], "inherit")
        for name in ("server-container.yml", "selfhost-release.yml"):
            called = yaml.safe_load((ROOT / ".github/workflows" / name).read_text())
            self.assertIn("workflow_call", called[True])
            self.assertIn("workflow_dispatch", called[True])
        # A called run inherits release.yml's group; sharing it would deadlock.
        group = yaml.safe_load((ROOT / ".github/workflows/selfhost-release.yml").read_text())["concurrency"]["group"]
        self.assertIn("github.workflow == 'Self-host installer'", group)
        self.assertIn("github.run_id", group)
        container = yaml.safe_load((ROOT / ".github/workflows/server-container.yml").read_text())
        public = container["jobs"]["public"]
        self.assertEqual(public["needs"], "publish")
        self.assertFalse(any("login" in step.get("uses", "") for step in public["steps"]))
        self.assertIn("visibility", public["steps"][0]["run"])


if __name__ == "__main__":
    unittest.main()
