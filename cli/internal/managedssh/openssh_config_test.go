//go:build darwin || linux

package managedssh

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestOpenSSHConfigInstallRepairAndExactUninstall(t *testing.T) {
	home := openSSHTestHome(t)
	directory := filepath.Join(home, ".ssh")
	original := []byte("# personal header\n\nHost *\n    ServerAliveInterval 30\n")
	if err := os.WriteFile(filepath.Join(directory, "config"), original, 0o600); err != nil {
		t.Fatal(err)
	}
	config := openSSHTestConfig(home)
	config.Targets = []OpenSSHAliasTarget{{Alias: "box", User: "pbt", Port: 22}}
	installed, err := InstallOpenSSHConfig(config)
	if err != nil || !installed.Changed {
		t.Fatalf("installed=%+v error=%v", installed, err)
	}
	if err := ValidateOpenSSHConfig(config); err != nil {
		t.Fatalf("validate installed config: %v", err)
	}
	if err := ValidateInstalledOpenSSHConfig(home, uint32(os.Getuid()), config.AgentSocket, config.AliasDomain); err != nil {
		t.Fatalf("validate daemon-installed config: %v", err)
	}
	alternate := config
	alternate.ProxyCommand = `"/opt/paperboat/pb" __ssh-proxy --host %h --port %p`
	alternate.KnownHostsCommand = `"/opt/paperboat/pb" __ssh-known-hosts --host %h --port %p`
	if err := ValidateOpenSSHConfig(alternate); !errors.Is(err, ErrOpenSSHConfigConflict) {
		t.Fatalf("path-specific validation error=%v", err)
	}
	if err := ValidateInstalledOpenSSHConfig(home, uint32(os.Getuid()), config.AgentSocket, config.AliasDomain); err != nil {
		t.Fatalf("path-independent validation error=%v", err)
	}
	main := readOpenSSHTestFile(t, filepath.Join(directory, "config"))
	includeOffset := strings.Index(string(main), openSSHIncludeMarker)
	hostOffset := strings.Index(string(main), "Host *")
	if includeOffset < 0 || hostOffset < 0 || includeOffset > hostOffset || strings.Count(string(main), openSSHIncludeMarker) != 1 {
		t.Fatalf("main config=%q", main)
	}
	owned := string(readOpenSSHTestFile(t, filepath.Join(directory, "paperboat_config")))
	for _, required := range []string{"Host *.machines.example.test", "Match host box.machines.example.test user \"pbt\"", "ProxyCommand " + config.ProxyCommand, "KnownHostsCommand " + config.KnownHostsCommand, "IdentityAgent \"" + config.AgentSocket + "\"", "IdentityFile \"" + config.IdentityFile + "\"", "IdentitiesOnly yes", "BatchMode yes", "PasswordAuthentication no", "KbdInteractiveAuthentication no", "StrictHostKeyChecking yes", "CheckHostIP no", "UserKnownHostsFile none", "GlobalKnownHostsFile none"} {
		if !strings.Contains(owned, required) {
			t.Fatalf("owned config missing %q: %q", required, owned)
		}
	}
	replay, err := InstallOpenSSHConfig(config)
	if err != nil || replay.Changed {
		t.Fatalf("replay=%+v error=%v", replay, err)
	}
	uninstalled, err := UninstallOpenSSHConfig(home, uint32(os.Getuid()))
	if err != nil || !uninstalled.Changed {
		t.Fatalf("uninstalled=%+v error=%v", uninstalled, err)
	}
	if final := readOpenSSHTestFile(t, filepath.Join(directory, "config")); string(final) != string(original) {
		t.Fatalf("final config=%q want=%q", final, original)
	}
	for _, name := range []string{"paperboat_config", ".paperboat-config-install-v1.json", ".paperboat-config-transaction-v1.json"} {
		if _, err := os.Lstat(filepath.Join(directory, name)); !os.IsNotExist(err) {
			t.Fatalf("%s remains after uninstall: %v", name, err)
		}
	}
	second, err := UninstallOpenSSHConfig(home, uint32(os.Getuid()))
	if err != nil || second.Changed {
		t.Fatalf("second uninstall=%+v error=%v", second, err)
	}
}

func TestOpenSSHConfigRendersCanonicalAliasPortWithoutUser(t *testing.T) {
	home := openSSHTestHome(t)
	config := openSSHTestConfig(home)
	config.Targets = []OpenSSHAliasTarget{{Alias: "victus-windows-e2e-fresh", User: "Pujan", Port: 38222}}
	if _, err := InstallOpenSSHConfig(config); err != nil {
		t.Fatal(err)
	}
	owned := string(readOpenSSHTestFile(t, filepath.Join(home, ".ssh", "paperboat_config")))
	want := "Host victus-windows-e2e-fresh.machines.example.test\n    Port 38222\n"
	if !strings.Contains(owned, want) {
		t.Fatalf("owned config missing authoritative target: %q", owned)
	}
	if strings.Contains(owned, "Victus-Windows-E2E-Fresh.machines.example.test") {
		t.Fatalf("managed config contains a noncanonical machine name: %q", owned)
	}
	if strings.Contains(owned, "\n    User ") {
		t.Fatalf("managed config must not override the invoking OpenSSH user: %q", owned)
	}
	if err := ValidateOpenSSHConfig(config); err != nil {
		t.Fatal(err)
	}
}

func TestOpenSSHConfigRemovesCreatedMainConfigOnUninstall(t *testing.T) {
	home := openSSHTestHome(t)
	if _, err := InstallOpenSSHConfig(openSSHTestConfig(home)); err != nil {
		t.Fatal(err)
	}
	if _, err := UninstallOpenSSHConfig(home, uint32(os.Getuid())); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".ssh", "config")); !os.IsNotExist(err) {
		t.Fatalf("created main config remains: %v", err)
	}
}

// previousOpenSSHFormat renders the fragment as the build before the per-user managed key wrote it: one
// wildcard block that forces the managed key on every user.
func previousOpenSSHFormat(config OpenSSHConfig) []byte {
	var out strings.Builder
	out.WriteString(openSSHBeginMarker + "\n")
	for _, target := range config.Targets {
		out.WriteString("Host " + target.Alias + "." + config.AliasDomain + "\n    Port " + strconv.Itoa(int(target.Port)) + "\n")
	}
	out.WriteString("Host *." + config.AliasDomain + "\n    ProxyCommand " + config.ProxyCommand + "\n    KnownHostsCommand " + config.KnownHostsCommand +
		"\n    IdentityAgent \"" + config.AgentSocket + "\"\n    IdentityFile \"" + config.IdentityFile + "\"\n" +
		"    IdentitiesOnly yes\n    BatchMode yes\n    PasswordAuthentication no\n    KbdInteractiveAuthentication no\n" +
		ownedOpenSSHTransport + openSSHEndMarker + "\n")
	return []byte(out.String())
}

func TestOpenSSHConfigAtomicallyUpgradesThePreviousManagedFragment(t *testing.T) {
	home := openSSHTestHome(t)
	config := openSSHTestConfig(home)
	config.Targets = []OpenSSHAliasTarget{{Alias: "mac", User: "adam", Port: 22}}
	if _, err := InstallOpenSSHConfig(config); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(home, ".ssh")
	previous := previousOpenSSHFormat(config)
	if !validPreviousOwnedOpenSSHContent(previous, config.AliasDomain) || validOwnedOpenSSHContent(previous, config.AliasDomain) {
		t.Fatal("test previous-format fragment is not recognized as the previous format")
	}
	var record openSSHInstallRecord
	recordPath := filepath.Join(directory, ".paperboat-config-install-v1.json")
	if err := json.Unmarshal(readOpenSSHTestFile(t, recordPath), &record); err != nil {
		t.Fatal(err)
	}
	record.OwnedHash = hashOpenSSHBytes(previous)
	recordJSON, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "paperboat_config"), previous, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordPath, append(recordJSON, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if ValidateOpenSSHConfig(config) == nil {
		t.Fatal("readiness accepted the previous format before the upgrade")
	}
	upgraded, err := InstallOpenSSHConfig(config)
	if err != nil || !upgraded.Changed {
		t.Fatalf("upgrade=%+v error=%v", upgraded, err)
	}
	if err := ValidateOpenSSHConfig(config); err != nil {
		t.Fatalf("validate upgraded config: %v", err)
	}
	if validPreviousOwnedOpenSSHContent(readOpenSSHTestFile(t, filepath.Join(directory, "paperboat_config")), config.AliasDomain) {
		t.Fatal("previous format remains after the upgrade")
	}
}

func TestGeneratedOpenSSHConfigIsAcceptedByInstalledClient(t *testing.T) {
	executable, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH client is not installed")
	}
	home := openSSHTestHome(t)
	config := openSSHTestConfig(home)
	config.Targets = []OpenSSHAliasTarget{{Alias: "probe", User: "pbt", Port: 22}}
	if _, err := InstallOpenSSHConfig(config); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-G", "-F", filepath.Join(home, ".ssh", "paperboat_config"), "-l", "pbt", "probe.machines.example.test")
	var stderr bytes.Buffer
	var stdout bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("OpenSSH rejected generated config: %v: %s", err, stderr.String())
	}
	effective := strings.ToLower(stdout.String())
	for _, required := range []string{"identitiesonly yes", "batchmode yes", "passwordauthentication no", "kbdinteractiveauthentication no"} {
		if !strings.Contains(effective, required+"\n") {
			t.Fatalf("OpenSSH effective config missing %q:\n%s", required, stdout.String())
		}
	}
}

func TestInstalledOpenSSHConfigResolvesEditorHostAlias(t *testing.T) {
	executable, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH client is not installed")
	}
	home := openSSHTestHome(t)
	config := openSSHTestConfig(home)
	config.Targets = []OpenSSHAliasTarget{{Alias: "editor-host", User: "remote-user", Port: 38222}}
	if _, err := InstallOpenSSHConfig(config); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-G", "-F", filepath.Join(home, ".ssh", "config"), "editor-host.machines.example.test")
	command.Env = openSSHTestEnvironment(home)
	var stderr bytes.Buffer
	var stdout bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("OpenSSH rejected the installed editor target: %v: %s", err, stderr.String())
	}
	effective := strings.ToLower(stdout.String())
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"user " + strings.ToLower(current.Username) + "\n", "port 38222\n", "canonicalizehostname true\n", "proxycommand \"/usr/local/bin/pb\" internal ssh-proxy --host %h --port %p\n"} {
		if !strings.Contains(effective, required) {
			t.Fatalf("effective editor target missing %q:\n%s", required, stdout.String())
		}
	}
	if strings.Contains(effective, "user remote-user\n") {
		t.Fatalf("plain OpenSSH unexpectedly inherited the target setup user:\n%s", stdout.String())
	}
}

// The managed key (pb's agent, the public key file, no password, no prompt) is configured only for the
// user who set pb up. Every other user connects through the same ProxyCommand and host-key trust and is
// left to OpenSSH's normal authentication: their own agent and keys, a password, a prompt.
func TestInstalledOpenSSHConfigOffersTheManagedKeyOnlyToTheSetupUser(t *testing.T) {
	executable, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH client is not installed")
	}
	home := openSSHTestHome(t)
	config := openSSHTestConfig(home)
	config.Targets = []OpenSSHAliasTarget{{Alias: "homelab", User: "pbt", Port: 22}}
	if _, err := InstallOpenSSHConfig(config); err != nil {
		t.Fatal(err)
	}
	effective := func(user string) string {
		t.Helper()
		command := exec.Command(executable, "-G", "-F", filepath.Join(home, ".ssh", "config"), "-l", user, "homelab.machines.example.test")
		command.Env = openSSHTestEnvironment(home)
		var stderr bytes.Buffer
		var stdout bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		if err := command.Run(); err != nil {
			t.Fatalf("OpenSSH rejected the installed target for %s: %v: %s", user, err, stderr.String())
		}
		return strings.ToLower(stdout.String())
	}
	managed := effective("pbt")
	for _, required := range []string{"identitiesonly yes\n", "batchmode yes\n", "passwordauthentication no\n", "kbdinteractiveauthentication no\n", "identityagent " + strings.ToLower(config.AgentSocket) + "\n", "identityfile " + strings.ToLower(config.IdentityFile) + "\n", "port 22\n", "proxycommand \"/usr/local/bin/pb\" internal ssh-proxy"} {
		if !strings.Contains(managed, required) {
			t.Fatalf("setup user is missing %q:\n%s", required, managed)
		}
	}
	other := effective("bob")
	for _, required := range []string{"user bob\n", "port 22\n", "proxycommand \"/usr/local/bin/pb\" internal ssh-proxy", "knownhostscommand ", "stricthostkeychecking true\n"} {
		if !strings.Contains(other, required) {
			t.Fatalf("another user does not get Paperboat's transport and host-key trust, missing %q:\n%s", required, other)
		}
	}
	for _, forbidden := range []string{"identitiesonly yes", "batchmode yes", "passwordauthentication no", "kbdinteractiveauthentication no", strings.ToLower(config.AgentSocket), strings.ToLower(config.IdentityFile)} {
		if strings.Contains(other, forbidden) {
			t.Fatalf("another user is forced into the managed key (%q):\n%s", forbidden, other)
		}
	}
	if !strings.Contains(other, "passwordauthentication yes\n") || !strings.Contains(other, "batchmode no\n") {
		t.Fatalf("another user cannot use a password or a prompt:\n%s", other)
	}
}

func openSSHTestEnvironment(home string) []string {
	environment := make([]string, 0, len(os.Environ())+1)
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, "HOME=") {
			continue
		}
		environment = append(environment, value)
	}
	return append(environment, "HOME="+home)
}

func TestOpenSSHConfigRejectsConflictsSymlinksAndModifiedOwnedState(t *testing.T) {
	home := openSSHTestHome(t)
	directory := filepath.Join(home, ".ssh")
	conflicting := "Host *.machines.example.test\n    ProxyCommand /tmp/other-proxy %h %p\n"
	if err := os.WriteFile(filepath.Join(directory, "config"), []byte(conflicting), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := InstallOpenSSHConfig(openSSHTestConfig(home))
	var optionConflict *OpenSSHOptionConflict
	if !errors.As(err, &optionConflict) || optionConflict.Line != 2 || optionConflict.Option != "ProxyCommand" || optionConflict.Existing != "/tmp/other-proxy %h %p" {
		t.Fatalf("option conflict=%+v error=%v", optionConflict, err)
	}
	if err := os.Remove(filepath.Join(directory, "config")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "outside-config")
	if err := os.WriteFile(target, []byte("Host outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(directory, "config")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := InstallOpenSSHConfig(openSSHTestConfig(home)); err == nil {
		t.Fatal("symlinked main config was accepted")
	}
	if string(readOpenSSHTestFile(t, target)) != "Host outside\n" {
		t.Fatal("symlink target was modified")
	}
	if err := os.Remove(filepath.Join(directory, "config")); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallOpenSSHConfig(openSSHTestConfig(home)); err != nil {
		t.Fatal(err)
	}
	ownedPath := filepath.Join(directory, "paperboat_config")
	if err := os.WriteFile(ownedPath, []byte("Host attacker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := UninstallOpenSSHConfig(home, uint32(os.Getuid())); !errors.Is(err, ErrOpenSSHConfigConflict) {
		t.Fatalf("modified owned state error=%v", err)
	}
}

func TestOpenSSHOptionConflictOmitsConfiguredCommandValues(t *testing.T) {
	err := &OpenSSHOptionConflict{Line: 12, Option: "ProxyCommand", Existing: "/tmp/private-agent --token secret", Required: "/home/private-user/pb --known-hosts"}
	if !errors.Is(err, ErrOpenSSHConfigConflict) {
		t.Fatalf("conflict sentinel lost: %v", err)
	}
	for _, private := range []string{"private-agent", "secret", "private-user", "known-hosts"} {
		if strings.Contains(err.Error(), private) {
			t.Fatalf("conflict error exposed configured value %q: %q", private, err)
		}
	}
	if got, want := err.Error(), "Paperboat OpenSSH configuration conflicts with existing state: line 12 contains a conflicting ProxyCommand"; got != want {
		t.Fatalf("conflict error=%q want=%q", got, want)
	}
	var classified interface {
		DiagnosticStage() string
		DiagnosticCode() string
	}
	if !errors.As(err, &classified) || classified.DiagnosticStage() != "command" || classified.DiagnosticCode() != "managed_ssh_failed" {
		t.Fatalf("conflict classification missing: %#v", err)
	}
}

func TestOpenSSHConfigBoundaryClassifiesValidationFailure(t *testing.T) {
	err := ValidateOpenSSHConfig(OpenSSHConfig{AliasDomain: testAliasDomain, Home: "relative-private-home"})
	if !errors.Is(err, ErrOpenSSHConfigConflict) {
		t.Fatalf("invalid home error=%v", err)
	}
	var classified interface {
		DiagnosticStage() string
		DiagnosticCode() string
	}
	if !errors.As(err, &classified) || classified.DiagnosticStage() != "command" || classified.DiagnosticCode() != "managed_ssh_failed" {
		t.Fatalf("configuration validation classification missing: %T %v", err, err)
	}
	if strings.Contains(err.Error(), "relative-private-home") {
		t.Fatalf("configuration error exposed path: %q", err)
	}
}

func TestOpenSSHConfigRecoversInterruptedTransaction(t *testing.T) {
	home := openSSHTestHome(t)
	directory := filepath.Join(home, ".ssh")
	config := openSSHTestConfig(home)
	if _, err := InstallOpenSSHConfig(config); err != nil {
		t.Fatal(err)
	}
	main := readOpenSSHTestFile(t, filepath.Join(directory, "config"))
	owned := readOpenSSHTestFile(t, filepath.Join(directory, "paperboat_config"))
	var record openSSHInstallRecord
	if err := json.Unmarshal(readOpenSSHTestFile(t, filepath.Join(directory, ".paperboat-config-install-v1.json")), &record); err != nil {
		t.Fatal(err)
	}
	transaction := openSSHTransaction{
		Version: 1, OriginalMain: encodeOpenSSHBytes(main), OriginalMainSet: true,
		OriginalOwned: encodeOpenSSHBytes(owned), OriginalOwnSet: true, OriginalRecord: &record,
		NextMain: encodeOpenSSHBytes(main), NextMainSet: true,
		NextOwned: encodeOpenSSHBytes([]byte("partial replacement\n")), NextOwnedSet: true,
	}
	journal, _ := json.Marshal(transaction)
	if err := os.WriteFile(filepath.Join(directory, ".paperboat-config-transaction-v1.json"), append(journal, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "paperboat_config"), []byte("partial replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := InstallOpenSSHConfig(config)
	if err != nil || recovered.Changed {
		t.Fatalf("recovered=%+v error=%v", recovered, err)
	}
	if value := readOpenSSHTestFile(t, filepath.Join(directory, "paperboat_config")); string(value) != string(owned) {
		t.Fatalf("recovered owned config=%q", value)
	}
	if _, err := os.Lstat(filepath.Join(directory, ".paperboat-config-transaction-v1.json")); !os.IsNotExist(err) {
		t.Fatalf("journal remains: %v", err)
	}
}

func openSSHTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

func openSSHTestConfig(home string) OpenSSHConfig {
	return OpenSSHConfig{AliasDomain: testAliasDomain,
		Home: home, OwnerUID: uint32(os.Getuid()),
		ProxyCommand:      "\"/usr/local/bin/pb\" internal ssh-proxy --host %h --port %p",
		KnownHostsCommand: "\"/usr/local/bin/pb\" internal ssh-known-hosts --host %h --port %p",
		AgentSocket:       filepath.Join(home, ".paperboat", "run", "ssh-agent.sock"),
		IdentityFile:      ManagedIdentityPublicKeyPath(home),
	}
}

func readOpenSSHTestFile(t *testing.T, path string) []byte {
	t.Helper()
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
