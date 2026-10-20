"""The pre-push hook refuses a pushed commit that does not build on a clean checkout."""
import subprocess
import tempfile
import unittest
from pathlib import Path

HOOK = Path(__file__).resolve().parent / "pre-push"


def git(repo, *args):
    return subprocess.run(["git", "-C", str(repo), *args], check=True, capture_output=True, text=True).stdout.strip()


class PrePushTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.repo = Path(self.directory.name)
        git(self.repo, "init", "-q", "-b", "main")
        git(self.repo, "config", "user.email", "t@example.test")
        git(self.repo, "config", "user.name", "t")
        self.write("cli/go.mod", "module example.test/cli\n\ngo 1.21\n")
        self.write("cli/a/a.go", "package a\n\nfunc A() int { return 1 }\n")
        self.write("README.md", "x\n")
        self.base = self.commit("base")

    def write(self, path, text):
        file = self.repo / path
        file.parent.mkdir(parents=True, exist_ok=True)
        file.write_text(text)

    def commit(self, message):
        git(self.repo, "add", "-A")
        git(self.repo, "commit", "-q", "-m", message)
        return git(self.repo, "rev-parse", "HEAD")

    def push(self, local, remote="0" * 40):
        return subprocess.run([str(HOOK)], cwd=self.repo, input=f"refs/heads/main {local} refs/heads/main {remote}\n",
                              capture_output=True, text=True, env={"PATH": subprocess.os.environ["PATH"], "HOME": subprocess.os.environ["HOME"], "GOFLAGS": "-mod=mod", "GOCACHE": subprocess.os.environ.get("GOCACHE", "/tmp/pb-hook-gocache")})

    def test_a_commit_that_builds_passes(self):
        self.write("cli/a/b.go", "package a\n\nfunc B() int { return A() }\n")
        head = self.commit("add b")
        result = self.push(head, self.base)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_a_file_left_out_of_the_commit_is_refused_even_though_the_working_tree_builds(self):
        self.write("cli/a/b.go", "package a\n\nfunc B() int { return Helper() }\n")  # uses a helper that lives in an untracked file
        self.write("cli/a/helper.go", "package a\n\nfunc Helper() int { return 2 }\n")
        git(self.repo, "add", "cli/a/b.go")
        git(self.repo, "commit", "-q", "-m", "forgot helper.go")
        head = git(self.repo, "rev-parse", "HEAD")
        self.assertEqual(subprocess.run(["go", "build", "./..."], cwd=self.repo / "cli", capture_output=True).returncode, 0, "the working tree builds")
        result = self.push(head, self.base)
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("go build ./... failed", result.stderr)
        self.assertIn("undefined: Helper", result.stderr)

    def test_a_vet_failure_in_a_changed_package_is_refused(self):
        self.write("cli/a/b.go", 'package a\n\nimport "fmt"\n\nfunc B() { fmt.Printf("%d", "x") }\n')
        head = self.commit("bad format")
        result = self.push(head, self.base)
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("go vet failed", result.stderr)

    def test_a_push_that_touches_no_module_is_instant_and_deleting_a_ref_is_ignored(self):
        self.write("README.md", "docs only\n")
        head = self.commit("docs")
        result = self.push(head, self.base)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("building", result.stderr)
        self.assertEqual(self.push("0" * 40, head).returncode, 0)


if __name__ == "__main__":
    unittest.main()
