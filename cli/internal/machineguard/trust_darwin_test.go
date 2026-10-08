//go:build darwin

package machineguard

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureDarwinSystemTrustAddsOnceAndReusesExactRoot(t *testing.T) {
	_, _, rootPEM := renewalFixture(t, 365)
	if _, err := parseLocalTrustRoot(rootPEM); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	pemPath := filepath.Join(directory, "root.pem")
	if err := os.WriteFile(pemPath, rootPEM, 0644); err != nil {
		t.Fatal(err)
	}
	// The certificate is already in System.keychain, but its SSL trust setting
	// is initially absent. Presence alone must trigger an explicit trust update.
	trusted := false
	adds := 0
	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "/usr/bin/security" {
			t.Fatalf("unexpected command %q", name)
		}
		switch args[0] {
		case "verify-cert":
			if len(args) != 7 || args[1] != "-p" || args[2] != "ssl" || args[3] != "-c" || args[4] != pemPath || args[5] != "-k" || args[6] != darwinSystemKeychain {
				t.Fatalf("verification did not use the normal SSL trust policy: %v", args)
			}
			if trusted {
				return nil, nil
			}
			return []byte("root present without SSL trust"), errors.New("certificate untrusted")
		case "add-trusted-cert":
			if args[len(args)-1] != pemPath {
				t.Fatalf("unexpected certificate path: %v", args)
			}
			adds++
			trusted = true
			return nil, nil
		default:
			t.Fatalf("unexpected security operation %q", args[0])
			return nil, nil
		}
	}
	for index := 0; index < 2; index++ {
		if err := ensureDarwinSystemTrust(t.Context(), pemPath, runner); err != nil {
			t.Fatal(err)
		}
	}
	if adds != 1 {
		t.Fatalf("security add-trusted-cert called %d times; want once", adds)
	}
	receipt, err := os.ReadFile(pemPath + ".installed")
	if err != nil || string(receipt) != "installed\n" {
		t.Fatalf("ownership receipt = %q, %v", receipt, err)
	}
}

// No real keychain operations: the root-owned state and manifest checks remain
// authentic, and only the security command boundary is injected.
func TestDarwinPartialTrustRetirementPreservesOtherNamespace(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned temporary guard state")
	}
	state := t.TempDir()
	cfg := Config{StateDir: state}
	retained, err := prepareNamespaceCA(state, "local.pprbt.dev", false)
	if err != nil {
		t.Fatal(err)
	}
	retired, err := prepareNamespaceCA(state, "custom.example", false)
	if err != nil {
		t.Fatal(err)
	}
	manifests := filepath.Join(state, "trusted-ca")
	if err := protectedDirectory(manifests, 0700); err != nil {
		t.Fatal(err)
	}
	pathFor := func(data []byte) string {
		t.Helper()
		root, err := parseLocalTrustRoot(data)
		if err != nil {
			t.Fatal(err)
		}
		return filepath.Join(manifests, localTrustFingerprint(root)+".pem")
	}
	for _, root := range [][]byte{retained.CertPEM(), retired.CertPEM()} {
		path := pathFor(root)
		if err := writeOwnedTrustState(path, root, 0644); err != nil {
			t.Fatal(err)
		}
		if err := recordDarwinTrustOwnership(path); err != nil {
			t.Fatal(err)
		}
	}
	target, err := parseLocalTrustRoot(retired.CertPEM())
	if err != nil {
		t.Fatal(err)
	}
	digest := sha1.Sum(target.Raw)
	fingerprint := strings.ToUpper(hex.EncodeToString(digest[:]))
	deletes := 0
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "/usr/bin/security" {
			t.Fatalf("unexpected executable %s", name)
		}
		switch args[0] {
		case "find-certificate":
			return []byte("SHA-1 hash: " + fingerprint + "\n"), nil
		case "delete-certificate":
			if len(args) != 5 || args[3] != fingerprint {
				t.Fatalf("retired wrong trust: %v", args)
			}
			deletes++
			return nil, nil
		default:
			t.Fatalf("unexpected operation %v", args)
			return nil, nil
		}
	}
	// Foreign manifests are still preserved; recognition of a second owned root
	// must not weaken validation of the protected directory.
	foreign := filepath.Join(manifests, "foreign.pem")
	if err := os.WriteFile(foreign, []byte("foreign"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := removeDarwinLocalTrust(t.Context(), cfg, retired.CertPEM(), run); err == nil || deletes != 0 {
		t.Fatal("foreign manifest was ignored or trust changed")
	}
	if err := os.Remove(foreign); err != nil {
		t.Fatal(err)
	}
	if err := removeDarwinLocalTrust(t.Context(), cfg, retired.CertPEM(), run); err != nil {
		t.Fatal(err)
	}
	if deletes != 1 {
		t.Fatalf("retired %d roots", deletes)
	}
	for _, path := range []string{pathFor(retained.CertPEM()), pathFor(retained.CertPEM()) + ".installed", filepath.Join(state, "certificates", "local.pprbt.dev", "rootCA.pem"), filepath.Join(state, "certificates", "custom.example", "rootCA.pem")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("unrelated trust or caller-owned CA state removed: %s %v", path, err)
		}
	}
	for _, path := range []string{pathFor(retired.CertPEM()), pathFor(retired.CertPEM()) + ".installed"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("retired manifest remains: %s %v", path, err)
		}
	}
}
