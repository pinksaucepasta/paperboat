"""The fixed self-host installer (helper/deploy/install.sh) against a signed fake GitHub release.

The installer must trust nothing before the release signature verifies; a verified release reaches `pbh check-host`
(the first step after the package is unpacked), which the fixture package stops with exit 77.
"""
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
INSTALLER = ROOT / "helper" / "deploy" / "install.sh"
SIGNER = ROOT / "cli" / "tools" / "sign-release-assets.sh"
VERSION = "2026.11.18.1"
REPO = "example/paperboat"
CURL = """#!/bin/sh
out= url=
while [ "$#" -gt 0 ]; do case "$1" in -o) out=$2; shift 2;; --connect-timeout|--max-time|--max-filesize|--proto|--proto-redir) shift 2;; -*) shift;; *) url=$1; shift;; esac; done
printf '%s\\n' "$url" >> "$FAKE_LOG"
case "$url" in
  https://api.github.com/repos/$FAKE_REPO/git/matching-refs/tags/selfhost-) cp "$FAKE_RELEASES/refs.json" "$out" ;;
  https://download.docker.com/linux/*/gpg) printf 'docker key placeholder\\n' > "$out" ;;
  https://github.com/$FAKE_REPO/releases/download/*/*) f=${url##*/}; v=${url%/*}; v=${v##*/}; [ -f "$FAKE_RELEASES/$v/$f" ] || exit 22; cp "$FAKE_RELEASES/$v/$f" "$out" ;;
  *) exit 22 ;;
esac
"""
SHIMS = {
    "uname": "#!/bin/sh\ncase \"$1\" in -s) echo Linux;; -m) echo x86_64;; esac\n",
    "id": "#!/bin/sh\necho 0\n",
    "systemctl": "#!/bin/sh\nexit 0\n",
    "timeout": "#!/bin/sh\nshift; exec \"$@\"\n",
    "install": "#!/bin/sh\nexit 0\n",
}


def have(tool):
    return shutil.which(tool) is not None


@unittest.skipUnless(have("ssh-keygen") and have("sha256sum"), "ssh-keygen and sha256sum are required")
class SelfhostFixture(unittest.TestCase):
    def setUp(self):
        self.root = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.root, True)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.releases = self.root / "releases"
        (self.releases / f"selfhost-{VERSION}").mkdir(parents=True)
        subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(self.root / "key")], check=True)
        public = " ".join((self.root / "key.pub").read_text().split()[:2])
        body = INSTALLER.read_text()
        lines = [f"allowed_signers='paperboat-release namespaces=\"paperboat-release\" {public}'" if l.startswith("allowed_signers=") else l for l in body.splitlines()]
        self.installer = self.root / "install.sh"
        self.installer.write_text("\n".join(lines) + "\n")
        self.release = self.releases / f"selfhost-{VERSION}"
        self.package(b"#!/bin/sh\necho reached-check-host >&2\nexit 77\n")
        (self.releases / "refs.json").write_text(f'[{{"ref": "refs/tags/selfhost-2026.10.01.0"}}, {{"ref": "refs/tags/selfhost-{VERSION}"}}]\n')
        for name, body in {"curl": CURL, **SHIMS}.items():
            (self.bin / name).write_text(body)
            (self.bin / name).chmod(0o755)

    def package(self, pbh):
        work = self.root / "pkg"
        shutil.rmtree(work, True)
        work.mkdir()
        (work / "pbh").write_bytes(pbh)
        for name in ("LICENSE", "tailscale.LICENSE", "manifest.json"):
            (work / name).write_text("x\n")
        (work / "pbh").chmod(0o755)
        subprocess.run(["tar", "-czf", str(self.release / "paperboat-selfhost-linux-amd64.tar.gz"), "-C", str(work), "pbh", "LICENSE", "tailscale.LICENSE", "manifest.json"], check=True)
        self.sign()

    def sign(self, tag=None, directory=None):
        directory = directory or self.release
        subprocess.run(["sh", str(SIGNER), str(directory), tag or f"selfhost-{VERSION}", str(self.root / "key"), str(self.installer)], check=True, capture_output=True)

    def run_installer(self, **env):
        full = {**os.environ, "PATH": f"{self.bin}:{os.environ['PATH']}", "TMPDIR": str(self.root), "FAKE_RELEASES": str(self.releases),
                "FAKE_REPO": REPO, "FAKE_LOG": str(self.root / "log"), "PB_REPO": REPO, "PB_VERSION": "", **env}
        return subprocess.run(["sh", str(self.installer)], env=full, capture_output=True, text=True, timeout=60)

    def log(self):
        path = self.root / "log"
        return path.read_text().split() if path.exists() else []


@unittest.skipUnless(have("ssh-keygen") and have("sha256sum"), "ssh-keygen and sha256sum are required")
class SelfhostInstallerTest(SelfhostFixture):
    def test_a_verified_release_reaches_the_package_and_picks_the_highest_version(self):
        result = self.run_installer()
        self.assertEqual(result.returncode, 77, result.stderr)
        self.assertIn("reached-check-host", result.stderr)
        self.assertEqual(self.log()[0], f"https://api.github.com/repos/{REPO}/git/matching-refs/tags/selfhost-")
        self.assertIn(f"https://github.com/{REPO}/releases/download/selfhost-{VERSION}/SHA256SUMS.sig", self.log())

    def test_a_pinned_version_skips_the_tag_lookup(self):
        result = self.run_installer(PB_VERSION=VERSION)
        self.assertEqual(result.returncode, 77, result.stderr)
        self.assertFalse(any("matching-refs" in u for u in self.log()))

    def refuses(self, expect, **env):
        result = self.run_installer(**env)
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn(expect, result.stderr)
        self.assertNotIn("reached-check-host", result.stderr)
        self.assertFalse(any(u.endswith(".tar.gz") for u in self.log()) and "SHA256SUMS" not in expect and "integrity" not in expect)
        return result

    def test_edited_sums_are_refused_before_the_package_is_downloaded(self):
        sums = self.release / "SHA256SUMS"
        sums.write_text(sums.read_text().replace("0", "1", 1))
        self.refuses("release signature on SHA256SUMS is not valid")
        self.assertFalse(any(u.endswith(".tar.gz") for u in self.log()))

    def test_a_signature_from_another_key_is_refused(self):
        other = self.root / "other"
        subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(other)], check=True)
        (self.release / "SHA256SUMS.sig").unlink()
        subprocess.run(["ssh-keygen", "-q", "-Y", "sign", "-f", str(other), "-n", "paperboat-release", str(self.release / "SHA256SUMS")], check=True)
        self.refuses("release signature on SHA256SUMS is not valid")

    def test_a_signed_file_replayed_from_another_release_is_refused(self):
        older = self.releases / "selfhost-2026.10.01.0"
        older.mkdir()
        shutil.copy(self.release / "paperboat-selfhost-linux-amd64.tar.gz", older)
        self.sign("selfhost-2026.10.01.0", older)
        for name in ("SHA256SUMS", "SHA256SUMS.sig"):
            shutil.copy(older / name, self.release / name)
        self.refuses("signed for a different release")

    def test_a_tampered_package_fails_the_hash_check_before_it_is_unpacked(self):
        with open(self.release / "paperboat-selfhost-linux-amd64.tar.gz", "ab") as f:
            f.write(b"tamper")
        self.refuses("Release integrity check failed")

    def test_no_published_release_is_a_clear_error(self):
        (self.releases / "refs.json").write_text("[]\n")
        self.refuses("has no published self-host release yet")

    def test_a_bad_version_is_refused(self):
        self.refuses("is not a release version", PB_VERSION="latest")
        self.refuses("is not a release version", PB_VERSION="2026.11.18.01")

    def test_missing_ssh_keygen_is_installed_through_the_package_manager_or_refused(self):
        keygen = shutil.which("ssh-keygen")
        limited = self.root / "limited"
        limited.mkdir()
        for tool in ("awk", "sed", "wc", "tr", "cat", "mktemp", "rm", "cp", "ls", "dirname", "basename", "grep", "head", "sort", "tail", "sha256sum", "tar", "env", "ln", "sh", "mkdir", "printf", "cut", "chmod", "gzip", "mv"):
            if shutil.which(tool):
                os.symlink(shutil.which(tool), limited / tool)
        for name in ("curl", *SHIMS):
            shutil.copy(self.bin / name, limited / name)
        (limited / "apt-get").write_text(f"#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$PM_LOG\"\ncase \"$*\" in *install*) ln -sf {keygen} {limited}/ssh-keygen ;; esac\n")
        (limited / "apt-get").chmod(0o755)
        full = {**os.environ, "PATH": str(limited), "TMPDIR": str(self.root), "FAKE_RELEASES": str(self.releases), "FAKE_REPO": REPO,
                "FAKE_LOG": str(self.root / "log"), "PB_REPO": REPO, "PB_VERSION": "", "PM_LOG": str(self.root / "pm")}
        result = subprocess.run([shutil.which("sh"), str(self.installer)], env=full, capture_output=True, text=True, timeout=60)
        self.assertEqual(result.returncode, 77, result.stderr)
        self.assertEqual((self.root / "pm").read_text().split("\n")[:2], ["update -qq", "install -y -qq openssh-client"])
        (limited / "apt-get").unlink()
        (limited / "ssh-keygen").unlink()
        result = subprocess.run([shutil.which("sh"), str(self.installer)], env=full, capture_output=True, text=True, timeout=60)
        self.assertEqual(result.returncode, 1)
        self.assertIn("could not be installed here", result.stderr)
        self.assertNotIn("reached-check-host", result.stderr)


DOCKER_FINGERPRINT = "9DC858229FC7DD38854AE2D88D81803C0EBFCD88"


def host_release():
    try:
        with open("/etc/os-release") as handle:
            values = dict(line.rstrip("\n").split("=", 1) for line in handle if "=" in line)
    except OSError:
        return "", ""
    return values.get("ID", "").strip('"'), (values.get("UBUNTU_CODENAME") or values.get("VERSION_CODENAME") or "").strip('"')


@unittest.skipUnless(have("ssh-keygen") and have("sha256sum") and host_release()[0] in ("ubuntu", "debian"), "needs ssh-keygen and a Debian or Ubuntu host (the installer reads /etc/os-release)")
class SelfhostInstallerDockerTest(SelfhostFixture):
    """Docker comes from Docker's own apt repository, pinned to its key's published fingerprint; no downloaded script runs."""

    def setUp(self):
        super().setUp()
        self.pbh_ok()
        self.limited = self.root / "limited"
        self.limited.mkdir()
        for tool in ("awk", "sed", "wc", "tr", "cat", "mktemp", "rm", "cp", "ls", "dirname", "basename", "grep", "head", "sort", "tail", "sha256sum", "tar", "env", "ln", "sh", "mkdir", "printf", "cut", "chmod", "gzip", "mv", "ssh-keygen"):
            if shutil.which(tool):
                os.symlink(shutil.which(tool), self.limited / tool)
        for name in ("curl", "id", "systemctl", "timeout", "uname"):
            shutil.copy(self.bin / name, self.limited / name)
        self.shim("install", r'''printf 'install %s\n' "$*" >> "$PM_LOG"
if [ "$1" = -m ] && [ "$#" -eq 4 ] && [ "$3" != -d ]; then cp "$3" "$CAPTURE/$(basename "$4")"; fi
true''')
        self.shim("apt-get", 'printf \'apt-get %s\\n\' "$*" >> "$PM_LOG"')
        self.shim("dpkg", "echo arm64")
        self.shim("gpg", 'printf "tru::1:0:0:3:1:5\\npub:-:4096:1:8D81803C0EBFCD88:1:::-:::scESC::::::23::0:\\nfpr:::::::::%s:\\nuid:::::::::Docker Release:\\n" "$GPG_FINGERPRINT"')
        (self.root / "capture").mkdir()

    def pbh_ok(self):
        self.package(b"#!/bin/sh\nexit 0\n")

    def shim(self, name, body):
        (self.limited / name).write_text("#!/bin/sh\n" + body + "\n")
        (self.limited / name).chmod(0o755)

    def run_docker(self, fingerprint=DOCKER_FINGERPRINT):
        env = {**os.environ, "PATH": str(self.limited), "TMPDIR": str(self.root), "FAKE_RELEASES": str(self.releases), "FAKE_REPO": REPO,
               "FAKE_LOG": str(self.root / "log"), "PB_REPO": REPO, "PB_VERSION": VERSION, "PM_LOG": str(self.root / "pm"),
               "CAPTURE": str(self.root / "capture"), "GPG_FINGERPRINT": fingerprint}
        result = subprocess.run([shutil.which("sh"), str(self.installer)], env=env, capture_output=True, text=True, timeout=60)
        pm = (self.root / "pm").read_text().splitlines() if (self.root / "pm").exists() else []
        return result, pm

    def test_docker_is_installed_from_dockers_apt_repository_with_the_pinned_key(self):
        os_id, codename = host_release()
        family = "ubuntu" if os_id == "ubuntu" or os.environ.get("UBUNTU_CODENAME") else "debian"
        result, pm = self.run_docker()
        self.assertEqual(result.returncode, 1, result.stderr)  # stops at `docker info`: no daemon here
        self.assertIn("Docker daemon is unavailable", result.stderr)
        apt = [line for line in pm if line.startswith("apt-get")]
        self.assertEqual(apt, ["apt-get update -qq", "apt-get install -y -qq ca-certificates curl gnupg", "apt-get update -qq",
                               "apt-get install -y -qq docker-ce docker-ce-cli containerd.io"])
        self.assertIn("install -m 0755 -d /etc/apt/keyrings", pm)
        listing = (self.root / "capture" / "docker.list").read_text()
        self.assertRegex(listing, rf"^deb \[arch=arm64 signed-by=/etc/apt/keyrings/docker.asc\] https://download.docker.com/linux/{family} {codename} stable\n$")
        self.assertTrue((self.root / "capture" / "docker.asc").exists())
        self.assertFalse(any("get.docker.com" in url for url in self.log()), "a Docker install script was downloaded")
        self.assertIn("https://download.docker.com/linux/%s/gpg" % family, self.log())

    def test_a_key_that_is_not_dockers_is_refused_before_anything_is_installed(self):
        result, pm = self.run_docker(fingerprint="0" * 40)
        self.assertEqual(result.returncode, 1)
        self.assertIn("does not match its published fingerprint", result.stderr)
        self.assertFalse(any(line.startswith("install ") for line in pm), pm)
        self.assertNotIn("apt-get install -y -qq docker-ce docker-ce-cli containerd.io", pm)

    def test_an_installed_docker_that_is_new_enough_is_left_alone(self):
        self.shim("docker", 'case "$1" in --version) echo "Docker version 27.5.1, build abc" ;; info) exit 1 ;; esac')
        result, pm = self.run_docker()
        self.assertIn("Docker daemon is unavailable", result.stderr)
        self.assertEqual(pm, [], "something was installed although Docker is already there")
        for version in ("20.10.24", "23.0.1", "28.1.0"):
            self.shim("docker", f'case "$1" in --version) echo "Docker version {version}, build x" ;; info) exit 1 ;; esac')
            self.assertIn("Docker daemon is unavailable", self.run_docker()[0].stderr, version)

    def test_a_docker_that_is_too_old_or_unreadable_is_refused(self):
        for output, message in (("Docker version 19.03.15, build x", "older than 20.10"), ("Docker version 20.9.1, build x", "older than 20.10"), ("podman emulation", "Could not read the installed Docker version")):
            self.shim("docker", f'case "$1" in --version) echo "{output}" ;; esac')
            result, pm = self.run_docker()
            self.assertEqual(result.returncode, 1, output)
            self.assertIn(message, result.stderr)
            self.assertEqual(pm, [])

    def test_without_apt_the_installer_points_at_dockers_documentation(self):
        (self.limited / "apt-get").unlink()
        result, pm = self.run_docker()
        self.assertEqual(result.returncode, 1)
        self.assertIn("docs.docker.com/engine/install", result.stderr)
        self.assertEqual(pm, [])


class SelfhostFlagsTest(unittest.TestCase):
    def test_flags_prompts_and_no_terminal_failure(self):
        result = subprocess.run(["bash", str(ROOT / "helper" / "deploy" / "test-install-args.sh")],
                                capture_output=True, text=True, timeout=120)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
