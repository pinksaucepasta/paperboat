package tools

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func installerSource(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ToLower(string(body))
}

func requireInOrder(t *testing.T, body string, values ...string) {
	t.Helper()
	position := 0
	for _, value := range values {
		next := strings.Index(body[position:], strings.ToLower(value))
		if next < 0 {
			t.Fatalf("installer is missing %q after byte %d", value, position)
		}
		position += next + len(value)
	}
}

// The installers are fixed files for every release: nothing in them is filled in per release, and neither names a
// Paperboat server.
func TestInstallersAreFixedAndNameNoServer(t *testing.T) {
	for _, name := range []string{"install.sh", "install.ps1"} {
		body := installerSource(t, name)
		if regexp.MustCompile(`@paperboat_[a-z0-9_]+@`).MatchString(body) {
			t.Fatalf("%s still carries a per-release placeholder", name)
		}
		// The only hosts an installer talks to are GitHub (releases, tag listing) and Docker's official installer.
		for _, host := range regexp.MustCompile(`https://([a-z0-9.-]+)`).FindAllStringSubmatch(body, -1) {
			if host[1] != "github.com" && host[1] != "api.github.com" && host[1] != "get.docker.com" {
				t.Fatalf("%s talks to %s; it must name no Paperboat server", name, host[1])
			}
		}
		if strings.Contains(body, "/tuf") || strings.Contains(body, "installer_base_url") {
			t.Fatalf("%s depends on the Paperboat release origin", name)
		}
		if !strings.Contains(body, "pinksaucepasta/paperboat") {
			t.Fatalf("%s has no repository constant", name)
		}
	}
}

func TestShellInstallerVerifiesTheSignatureBeforeTrustingAnyHash(t *testing.T) {
	body := installerSource(t, "install.sh")
	for _, required := range []string{"--proto-redir '=https'", "releases/download", "product digest mismatch", `"$installer_pb" install --install-dir "$install_dir" --json`,
		"data.executable", "--pair requires --enrollment-token", "umask 077", "--enrollment-token-file", "resume_executable", `"$target" pair`, `"$target" setup`,
		"-n paperboat-release", "ssh-keygen -y verify"} {
		if !strings.Contains(body, required) {
			t.Fatalf("shell installer is missing %q", required)
		}
	}
	for _, removed := range []string{"__install", "--source", "--skip", "--fresh", "cleanup_existing", "sudo installer", "pb-bootstrap", "paperboat_bootstrap", "--tuf-url", "paperboat_recipe_install", "paperboat_enrollment_token"} {
		if strings.Contains(body, removed) {
			t.Fatalf("shell installer retains obsolete behavior %q", removed)
		}
	}
	requireInOrder(t, body, "SHA256SUMS.sig", "ssh-keygen -Y verify", "signed for a different release", "does not list", "product digest mismatch", `"$installer_pb" install --install-dir "$install_dir" --json`, "data.executable")
	requireInOrder(t, body, `"$installer_pb" reset`, `"$installer_pb" install`)
	if strings.Index(body, `"$target" pair`) < strings.Index(body, "data.executable") {
		t.Fatal("shell installer pairs before invoking the installed binary")
	}
}

func TestShellInstallerParses(t *testing.T) {
	if output, err := exec.Command("sh", "-n", "install.sh").CombinedOutput(); err != nil {
		t.Fatalf("sh -n install.sh: %v\n%s", err, output)
	}
}

func TestShellInstallerSkipsTheServiceResetWithoutSystemd(t *testing.T) {
	body := installerSource(t, "install.sh")
	for _, required := range []string{"/run/systemd/system", `[ "$pair" = true ] && [ "$no_systemd" = false ]`} {
		if !strings.Contains(body, required) {
			t.Fatalf("shell installer is missing %q", required)
		}
	}
	requireInOrder(t, body, "no_systemd=true", `"$installer_pb" reset`)
}

// Every installer and the selfhost installer embed the one release public key; the signing helper proves it.
func TestInstallersEmbedTheSameReleaseKey(t *testing.T) {
	key := regexp.MustCompile(`paperboat-release namespaces="paperboat-release" ssh-ed25519 [A-Za-z0-9+/=]+`)
	var first string
	for _, name := range []string{"install.sh", "install.ps1", "../../helper/deploy/install.sh"} {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		found := key.FindAllString(string(body), -1)
		if len(found) != 1 {
			t.Fatalf("%s embeds %d release keys, want 1", name, len(found))
		}
		if first == "" {
			first = found[0]
		} else if found[0] != first {
			t.Fatalf("%s embeds a different release key", name)
		}
	}
}
