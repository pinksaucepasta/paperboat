package hostruntimecmd

import (
	"bytes"
	"encoding/base64"
	"net"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/adrg/xdg"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/runtimeport"
)

func TestFreshBootstrapCannotReplaceExistingEnrollment(t *testing.T) {
	root := t.TempDir()
	if runtime.GOOS == "windows" {
		// Windows bootstrap refuses a custom state root, so its default one is pointed into the test directory.
		previous := xdg.StateHome
		xdg.StateHome = root
		t.Cleanup(func() { xdg.StateHome = previous })
		root = filepath.Join(root, "Paperboat", "runtime")
	}
	store, err := identity.Open(identity.Config{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	key := store.Current()
	if err := store.SaveRegistration(identity.Registration{ServerURL: "https://api.example.test", MachineID: "machine_1", EnvironmentID: "env_1", PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: filepath.Join(root, "inbox"), InstallationGeneration: 1, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := rejectFreshBootstrapOverEnrollment(store, bootstrap.ErrResumeNotFound); err == nil || !strings.Contains(err.Error(), "pb uninstall") || !strings.Contains(err.Error(), "pb setup --name <machine-alias>") {
		t.Fatalf("guard error=%v", err)
	}
	var output, failure bytes.Buffer
	code := Execute(t.Context(), []string{"bootstrap", "--server", "https://api.example.test", "--name", "test-machine", "--state-root", root}, strings.NewReader(""), &output, &failure)
	if code == 0 || !strings.Contains(failure.String(), "pb setup --name <machine-alias>") {
		t.Fatalf("existing enrollment refusal lost recovery: code=%d output=%q", code, failure.String())
	}
	registration, err := store.Registration()
	if err != nil || registration.MachineID != "machine_1" || registration.PublicKeyID != key.ID {
		t.Fatal("refused pairing changed existing enrollment")
	}
	if err := rejectFreshBootstrapOverEnrollment(store, nil); err != nil {
		t.Fatalf("resume was blocked: %v", err)
	}
}

// A host serves any number of OS users: each account starts at its own port and probes forward, so no count of users
// exhausts a fixed pair (the third Windows user of one host could not install with 38080 and 48080).
func TestBootstrapListenerPicksAFreePortPerUser(t *testing.T) {
	firstPorts := map[string]bool{}
	for _, seed := range []string{"S-1-5-21-1-1001", "S-1-5-21-1-1002", "S-1-5-21-1-1003", "1000", "1001", "1002"} {
		candidates := runtimeport.Candidates(seed)
		if len(candidates) != runtimeport.Attempts || candidates[0] != runtimeport.Candidates(seed)[0] {
			t.Fatalf("%s: %d candidates, first not stable", seed, len(candidates))
		}
		for _, address := range candidates {
			_, port, err := net.SplitHostPort(address)
			number, convErr := strconv.Atoi(port)
			if err != nil || convErr != nil || number < runtimeport.First || number >= runtimeport.First+runtimeport.Span {
				t.Fatalf("%s: candidate %q is outside the runtime port range", seed, address)
			}
		}
		firstPorts[candidates[0]] = true
	}
	if len(firstPorts) < 5 {
		t.Fatalf("users share their first port: %v", firstPorts)
	}
	free, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	freeAddress := free.Addr().String()
	if err := free.Close(); err != nil {
		t.Fatal(err)
	}
	taken, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	takenAddress := taken.Addr().String()
	if got, err := chooseBootstrapLoopbackAddress([]string{freeAddress, takenAddress}); err != nil || got != freeAddress {
		t.Fatalf("free first candidate: %q, %v", got, err)
	}
	if got, err := chooseBootstrapLoopbackAddress([]string{takenAddress, freeAddress}); err != nil || got != freeAddress {
		t.Fatalf("taken first candidate was not probed past: %q, %v", got, err)
	}
	got, err := chooseBootstrapLoopbackAddress([]string{takenAddress})
	if got != "" || err == nil || !strings.Contains(runtimeFailureMessage(err), "are in use") {
		t.Fatalf("occupied ports choice = %q, %v (message %q)", got, err, runtimeFailureMessage(err))
	}
}

func TestBootstrapListenerSurvivesCredentialConsumptionAndMaterialRecovery(t *testing.T) {
	root, now := t.TempDir(), time.Now().UTC()
	material := testClientBootstrapMaterial("https://control.example.test", now.Add(time.Hour))
	publicKey := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	record := bootstrap.NewResumeRecord(material.ControlURL, publicKey, "token", "Laptop", strings.Repeat("v", 40), now.Add(time.Hour))
	record.PairingStarted, record.Material = true, &material
	if err := prepareBootstrapListener(root, &material, &record, "test-user"); err != nil {
		t.Fatal(err)
	}
	address := material.HelperListenAddress
	material.EnrollmentCredential = ""
	record.RuntimeEnrolled = true
	if err := bootstrap.SaveResume(root, record); err != nil {
		t.Fatalf("checkpoint after credential consumption: %v", err)
	}
	reloaded, err := bootstrap.LoadResume(root, record.ServerURL, publicKey, "", "Laptop", now)
	if err != nil {
		t.Fatal(err)
	}
	// Fresh server material contains its default listener; the local journal
	// remains the authority for the already selected per-user address.
	recovered := testClientBootstrapMaterial(record.ServerURL, now.Add(time.Hour))
	reloaded.Material = &recovered
	if err := prepareBootstrapListener(root, &recovered, &reloaded, "test-user"); err != nil {
		t.Fatal(err)
	}
	if recovered.HelperListenAddress != address || reloaded.RuntimeListenAddress != address {
		t.Fatal("material recovery replaced the selected listener")
	}
	for _, invalid := range []string{"0.0.0.0:1234", "127.0.0.1:0", "127.0.0.1:http"} {
		reloaded.RuntimeListenAddress = invalid
		if err := bootstrap.SaveResume(root, reloaded); err == nil {
			t.Fatalf("accepted invalid listener %q", invalid)
		}
	}
}
