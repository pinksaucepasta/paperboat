package tools

import (
	"os"
	"os/exec"
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

func TestShellInstallerVerifiesBeforeRunningPublicInstall(t *testing.T) {
	body := installerSource(t, "install.sh")
	for _, required := range []string{
		"@paperboat_bootstrap_linux_amd64_sha256@", "--proto-redir '=https'", "releases/download", "bootstrap verifier length mismatch",
		"bootstrap verifier digest mismatch", `"$verifier" --tuf-url`, `"$installer_pb" install --install-dir "$install_dir" --json`, "data.executable", "pkgutil --expand",
		`$payload/library/privilegedhelpertools/paperboat/bin/pb`, "installer_pb=$payload_pb", "--pair requires --enrollment-token", "umask 077", "--enrollment-token-file", "resume_executable", `"$target" pair`, `"$target" setup`,
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("shell installer is missing %q", required)
		}
	}
	for _, removed := range []string{"__install", "--source", "--skip", "--fresh", "cleanup_existing", "sudo installer"} {
		if strings.Contains(body, removed) {
			t.Fatalf("shell installer retains obsolete behavior %q", removed)
		}
	}
	requireInOrder(t, body, "bootstrap verifier length mismatch", "bootstrap verifier digest mismatch", `"$verifier" --tuf-url`, `"$installer_pb" install --install-dir "$install_dir" --json`, "data.executable")
	requireInOrder(t, body, `"$installer_pb" reset`, `"$installer_pb" install`)
	if strings.Index(body, `"$target" pair`) < strings.Index(body, "data.executable") {
		t.Fatal("shell installer pairs before invoking the installed binary")
	}
}

func TestShellInstallerParses(t *testing.T) {
	command := exec.Command("sh", "-n", "install.sh")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("sh -n install.sh: %v\n%s", err, output)
	}
}

func TestWindowsInstallerVerifiesBeforeRunningPublicInstall(t *testing.T) {
	body := installerSource(t, "install.ps1")
	for _, required := range []string{
		"@paperboat_bootstrap_windows_amd64_sha256@", "'--proto-redir' '=https'", "releases/download", "bootstrap verifier length mismatch",
		"bootstrap verifier digest mismatch", "& $verifier '--tuf-url'", "invoke-paperboatinstall $download", "result.data.executable", "[io.path]::ispathrooted",
		"setsecuritydescriptorsddlform", "--enrollment-token-file", "resetresult.data.executable", "invoke-paperboat $installedpb @('pair'", "finally", "remove-item -literalpath $dir -recurse",
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("Windows installer is missing %q", required)
		}
	}
	for _, removed := range []string{"__install", "--source", "--skip", "--fresh", "invoke-freshpairrollback", "start-isolatedinstallerprocess", "-verb runas"} {
		if strings.Contains(body, removed) {
			t.Fatalf("Windows installer retains obsolete behavior %q", removed)
		}
	}
	requireInOrder(t, body, "bootstrap verifier length mismatch", "bootstrap verifier digest mismatch", "& $verifier '--tuf-url'", "invoke-paperboatinstall $download")
	requireInOrder(t, body, "$resetraw = & $download 'reset'", "if ($resume)", "invoke-paperboatinstall $download")
	if strings.Index(body, "invoke-paperboat $installedpb @('pair'") < strings.Index(body, "result.data.executable") {
		t.Fatal("Windows installer pairs before validating the returned installed executable")
	}
}
