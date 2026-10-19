"""Contract of the hardcoded-domain check."""
import unittest

import check_domains


class DomainCheckTest(unittest.TestCase):
    def test_reports_every_production_domain_in_code(self):
        found = check_domains.violations({
            "cli/a.go": 'x := "https://api.pprbt.dev"\ny := "ok.example.test"\n',
            "console/b.ts": "const u = 'https://app.paperboat.cloud'",
            "server/c_test.go": 'host := "3000.hp.ppr.boats"',
            "tunnel/d.go": 'h := "q.pprbt.app"',
            "deploy/e.yml": "origin: https://oracle.newt-mermaid.ts.net",
        })
        self.assertEqual([line.split(": ")[0] for line in found],
                         ["cli/a.go:1", "console/b.ts:1", "deploy/e.yml:1", "server/c_test.go:1", "tunnel/d.go:1"])

    def test_documentation_and_build_configuration_are_not_scanned(self):
        self.assertEqual(check_domains.violations({
            "README.md": "curl https://get.pprbt.dev/install | sh",
            "cli/docs/pb.1": "ppr.boats",
            "server/docs/runbooks/x.txt": "api.pprbt.dev",
            "cli/Makefile": "DEFAULT_SERVER_URL ?= https://api.pprbt.dev",
        }), [])


if __name__ == "__main__":
    unittest.main()
