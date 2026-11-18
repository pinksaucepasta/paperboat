package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// limitedTools gives the installer a PATH that holds only the tools it needs, so a test decides whether ssh-keygen, curl,
// sudo or a package manager exist. The fake release's own curl and uname are kept; id answers uid.
func limitedTools(t *testing.T, r *fakeRelease, uid string, missing ...string) string {
	t.Helper()
	dir := filepath.Join(r.root, "limited")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"awk", "sed", "wc", "tr", "cat", "mktemp", "rm", "chmod", "cp", "ls", "dirname", "basename", "hostname", "stat", "grep", "head", "sha256sum", "python3", "env", "ln", "sh", "mkdir"} {
		if path, err := exec.LookPath(tool); err == nil {
			_ = os.Symlink(path, filepath.Join(dir, tool))
		}
	}
	for _, name := range []string{"curl", "uname"} {
		body, _ := os.ReadFile(filepath.Join(r.bin, name))
		if name == "curl" && contains(missing, "curl") {
			continue
		}
		write(t, filepath.Join(dir, name), string(body), 0o700)
	}
	write(t, filepath.Join(dir, "id"), "#!/bin/sh\necho "+uid+"\n", 0o700)
	return dir
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// addPackageManager adds a fake package manager whose install step makes the real ssh-keygen appear, like a real package.
func addPackageManager(t *testing.T, dir, name string) {
	t.Helper()
	real, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen unavailable")
	}
	write(t, filepath.Join(dir, name), "#!/bin/sh\nprintf '%s %s\\n' \"$(basename \"$0\")\" \"$*\" >> \"$PM_LOG\"\ncase \"$*\" in *install*|add*|*-S*) ln -sf '"+real+"' \""+dir+"/ssh-keygen\" ;; esac\n", 0o700)
}

func addRootHelper(t *testing.T, dir, name string) {
	t.Helper()
	write(t, filepath.Join(dir, name), "#!/bin/sh\nprintf '%s\\n' \"$(basename \"$0\")\" >> \"$PM_LOG\"\nexec \"$@\"\n", 0o700)
}

// A computer without ssh-keygen gets it from its package manager first (NetBird and Tailscale install gnupg the same way),
// through sudo or doas unless already root; verification is never skipped.
func TestInstallerInstallsAMissingSshKeygenThroughThePackageManager(t *testing.T) {
	skipOnWindows(t)
	for _, c := range []struct {
		name, uid, manager string
		helper             string
		want               string
	}{
		{"apt as root", "0", "apt-get", "", "apt-get update -qq\napt-get install -y -qq openssh-client\n"},
		{"apt with sudo", "1000", "apt-get", "sudo", "sudo\napt-get update -qq\nsudo\napt-get install -y -qq openssh-client\n"},
		{"dnf with doas", "1000", "dnf", "doas", "doas\ndnf install -y openssh\n"},
		{"yum as root", "0", "yum", "", "yum install -y openssh\n"},
		{"apk as root", "0", "apk", "", "apk add --no-cache openssh-keygen\n"},
		{"pacman as root", "0", "pacman", "", "pacman -S --noconfirm openssh\n"},
		{"zypper as root", "0", "zypper", "", "zypper --non-interactive install openssh\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := macRelease(t)
			r.path = limitedTools(t, r, c.uid)
			addPackageManager(t, r.path, c.manager)
			if c.helper != "" {
				addRootHelper(t, r.path, c.helper)
			}
			log := filepath.Join(r.root, "pm-log")
			output, err := r.run(t, []string{"--no-setup"}, "PM_LOG="+log, "SCENARIO=ok", "CALLS="+filepath.Join(r.root, "calls"), "CANONICAL="+filepath.Join(r.root, "installed"))
			if err != nil {
				t.Fatalf("install failed: %v %s", err, output)
			}
			if got, _ := os.ReadFile(log); string(got) != c.want {
				t.Fatalf("package manager calls = %q, want %q", got, c.want)
			}
			if !strings.Contains(output, "installing OpenSSH") {
				t.Fatalf("the installer did not say it is installing OpenSSH: %s", output)
			}
		})
	}
}

// When ssh-keygen cannot be installed the installer stops; it never carries on unverified.
func TestInstallerStopsWhenSshKeygenCannotBeInstalled(t *testing.T) {
	skipOnWindows(t)
	for name, c := range map[string]struct {
		uid     string
		manager string
	}{
		"no package manager":        {"0", ""},
		"not root, no sudo or doas": {"1000", "apt-get"},
	} {
		t.Run(name, func(t *testing.T) {
			r := macRelease(t)
			r.path = limitedTools(t, r, c.uid)
			if c.manager != "" {
				addPackageManager(t, r.path, c.manager)
			}
			calls := filepath.Join(r.root, "calls")
			output, err := r.run(t, []string{"--no-setup"}, "PM_LOG="+filepath.Join(r.root, "pm-log"), "SCENARIO=ok", "CALLS="+calls)
			if err == nil || !strings.Contains(output, "ssh-keygen is required to verify the release signature and could not be installed here") {
				t.Fatalf("want a clear refusal: %v %s", err, output)
			}
			if _, statErr := os.Stat(calls); !os.IsNotExist(statErr) {
				t.Fatal("pb ran without a verified release")
			}
			for _, url := range r.downloads(t) {
				if strings.HasSuffix(url, "/pb-darwin-arm64") {
					t.Fatal("the product was downloaded although the signature could not be verified")
				}
			}
		})
	}
}

// Like Tailscale's installer (scripts/installer.sh 399-407): no download tool is an error that says what to install.
func TestInstallerAsksForCurlWhenItIsMissing(t *testing.T) {
	skipOnWindows(t)
	r := macRelease(t)
	r.path = limitedTools(t, r, "0", "curl")
	output, err := r.run(t, []string{"--no-setup"})
	if err == nil || !strings.Contains(output, "The installer needs curl to download files.") || !strings.Contains(output, "Please install curl to proceed.") {
		t.Fatalf("missing curl was not reported: %v %s", err, output)
	}
}
