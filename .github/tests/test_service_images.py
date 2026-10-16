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
                    "tunnel": "SENTRY_DSN_TUNNEL"}
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


if __name__ == "__main__":
    unittest.main()
