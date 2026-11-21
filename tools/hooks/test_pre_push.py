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

    def test_recent_main_lines_need_an_explicit_revert(self):
        self.write("console/usage.txt", "usage card\n")
        recent = self.commit("publish usage card")
        git(self.repo, "update-ref", "refs/remotes/origin/main", recent)

        self.write("server-fix.txt", "unrelated fix\n")
        self.write("console/usage.txt", "usage card\nnormal addition\n")
        normal = self.commit("ordinary update")
        result = self.push(normal, recent)
        self.assertEqual(result.returncode, 0, result.stderr)

        git(self.repo, "rm", "-q", "console/usage.txt")
        stale = self.commit("unrelated fix with a stale console tree")
        self.write("console/usage.txt", "usage card\nnormal addition\n")
        restored = self.commit("restore the card in a later commit")
        result = self.push(restored, recent)
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("console/usage.txt", result.stderr)
        self.assertIn(f"Reverts: {recent}", result.stderr)
        self.assertIn(stale[:10], result.stderr)

        git(self.repo, "reset", "--hard", stale)
        git(self.repo, "commit", "--amend", "--no-edit", "--trailer", f"Reverts: {recent}")
        intentional = git(self.repo, "rev-parse", "HEAD")
        result = self.push(intentional, recent)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_the_pushing_checkout_stays_on_its_branch(self):
        # Git sets GIT_DIR for hooks; with it, the build tree's forced checkout used to detach the
        # pushing checkout's HEAD at the pushed commit.
        self.write("cli/a/b.go", "package a\n\nfunc B() int { return A() }\n")
        head = self.commit("add b")
        self.write("cli/a/c.go", "package a\n\nfunc C() int { return A() }\n")
        second = self.commit("add c")
        self.write("cli/a/d.go", "package a\n\nfunc D() int { return A() }\n")
        self.commit("add d, not pushed")
        env = {"PATH": subprocess.os.environ["PATH"], "HOME": subprocess.os.environ["HOME"], "GOFLAGS": "-mod=mod",
               "GOCACHE": subprocess.os.environ.get("GOCACHE", "/tmp/pb-hook-gocache"), "GIT_DIR": str(self.repo / ".git")}
        for pushed, remote in ((head, self.base), (second, head)):  # the second push reuses the build tree
            result = subprocess.run([str(HOOK)], cwd=self.repo, input=f"refs/heads/main {pushed} refs/heads/main {remote}\n",
                                    capture_output=True, text=True, env=env)
            self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(git(self.repo, "symbolic-ref", "--short", "HEAD"), "main")
        self.assertEqual(git(self.repo, "status", "--porcelain"), "")

    def docs_package(self):
        # Stands in for cli/cmd/pb: its TestCLIReference fails while docs/stale exists.
        self.write("cli/cmd/pb/pb.go", "package main\n\nfunc main() {}\n")
        self.write("cli/cmd/pb/docs_test.go", 'package main\n\nimport ("os"; "testing")\n\nfunc TestCLIReference(t *testing.T) {\n\tif _, err := os.Stat("../../../docs/stale"); err == nil {\n\t\tt.Fatal("missing or stale; run make cli-docs")\n\t}\n}\n')
        self.write("docs/stale", "x\n")
        return self.commit("docs package")

    def test_help_text_changed_without_regenerated_docs_is_refused_before_push(self):
        base = self.docs_package()
        self.write("cli/cmd/pb/pb.go", "package main\n\nfunc main() { _ = \"new help\" }\n")
        head = self.commit("change help")
        result = self.push(head, base)
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("TestCLIReference failed; run `make cli-docs`", result.stderr)

    def test_the_docs_test_does_not_run_for_other_packages(self):
        base = self.docs_package()
        self.write("cli/a/b.go", "package a\n\nfunc B() int { return A() }\n")
        head = self.commit("add b")
        result = self.push(head, base)
        self.assertEqual(result.returncode, 0, result.stderr)

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
        self.assertIn("go build failed", result.stderr)
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

    def test_a_rebase_over_non_go_commits_is_not_rebuilt_but_a_go_change_is(self):
        self.write("cli/a/b.go", "package a\n\nfunc B() int { return A() }\n")
        first = self.commit("add b")
        self.assertEqual(self.push(first, self.base).returncode, 0)
        self.write("README.md", "someone else's docs commit\n")
        rebased = self.commit("docs")
        result = self.push(rebased, self.base)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("not rebuilt", result.stderr)
        self.write("cli/a/c.go", "package a\n\nfunc C() int { return Missing() }\n")
        broken = self.commit("go change")
        result = self.push(broken, self.base)
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("undefined: Missing", result.stderr)

    def test_a_package_whose_go_files_were_all_deleted_is_not_a_build_error(self):
        # Deleting cli/internal/controlsync/*.go while keeping its proto/ subpackage left a changed
        # directory with no Go files; `go build ./thatdir` failed and the hook refused a green push.
        self.write("cli/a/gone/g.go", "package gone\n\nfunc G() int { return 1 }\n")
        self.write("cli/a/gone/sub/s.go", "package sub\n\nfunc S() int { return 2 }\n")
        before = self.commit("add gone and gone/sub")
        git(self.repo, "rm", "-q", "cli/a/gone/g.go")
        head = self.commit("delete gone, keep gone/sub")
        result = self.push(head, before)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_a_directory_with_only_test_files_does_not_fail_the_build(self):
        self.write("cli/a/onlytest/x_test.go", 'package onlytest\n\nimport "testing"\n\nfunc TestX(t *testing.T) {}\n')
        head = self.commit("a package with only a test file")
        result = self.push(head, self.base)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_a_vet_failure_in_a_test_only_directory_is_still_refused(self):
        self.write("cli/a/onlytest/x_test.go", 'package onlytest\n\nimport (\n\t"fmt"\n\t"testing"\n)\n\nfunc TestX(t *testing.T) { fmt.Printf("%d", "x") }\n')
        head = self.commit("bad test")
        result = self.push(head, self.base)
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("go vet failed", result.stderr)

    def test_without_a_base_to_diff_against_the_whole_module_is_built(self):
        # A new branch in a clone without origin/main: nothing to diff, so every package is built.
        self.write("cli/a/b.go", "package a\n\nfunc B() int { return Missing() }\n")
        head = self.commit("broken")
        result = self.push(head)
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("undefined: Missing", result.stderr)

    def test_without_a_base_a_building_module_passes(self):
        self.assertEqual(self.push(self.base).returncode, 0)


if __name__ == "__main__":
    unittest.main()
