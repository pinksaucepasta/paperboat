//go:build linux

package machineguard

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxSystemTrustInstallActivatesAndIsIdempotent(t *testing.T) {
	_, _, rootPEM := renewalFixture(t, 365)
	root, err := parseLocalTrustRoot(rootPEM)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	trust := filepath.Join(directory, "trust")
	bundle := filepath.Join(directory, "bundle.pem")
	if err = os.Mkdir(trust, 0755); err != nil {
		t.Fatal(err)
	}
	runs := 0
	runner := func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if name != "update-ca-certificates" {
			t.Fatalf("unexpected command %q", name)
		}
		runs++
		return nil, os.WriteFile(bundle, rootPEM, 0644)
	}
	if err = installLinuxSystemTrustAt(t.Context(), trust, bundle, root, rootPEM, uint32(os.Geteuid()), runner); err != nil {
		t.Fatal(err)
	}
	if !linuxTrustBundleContains(bundle, root.Raw) {
		t.Fatal("installed root is not active in system trust bundle")
	}
	if err = installLinuxSystemTrustAt(t.Context(), trust, bundle, root, rootPEM, uint32(os.Geteuid()), runner); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("system trust updater ran %d times; want once", runs)
	}

	foreignPath := linuxOwnedTrustPath(trust, localTrustSuffix)
	if err = os.WriteFile(foreignPath, []byte("foreign certificate"), 0644); err != nil {
		t.Fatal(err)
	}
	if err = installLinuxSystemTrustAt(t.Context(), trust, bundle, root, rootPEM, uint32(os.Geteuid()), runner); err == nil || !strings.Contains(err.Error(), "foreign material") {
		t.Fatalf("foreign trust path was not rejected: %v", err)
	}
	contents, readErr := os.ReadFile(foreignPath)
	if readErr != nil || string(contents) != "foreign certificate" {
		t.Fatal("conflicting trust file was not preserved")
	}
}

type fakeNSSDatabase struct {
	certificates map[string][]byte
}

func (db *fakeNSSDatabase) run(_ context.Context, args ...string) ([]byte, error) {
	database := ""
	nickname := ""
	input := ""
	for index := 0; index+1 < len(args); index++ {
		switch args[index] {
		case "-d":
			database = strings.TrimPrefix(args[index+1], "sql:")
		case "-n":
			nickname = args[index+1]
		case "-i":
			input = args[index+1]
		}
	}
	if database == "" {
		return nil, errors.New("missing database")
	}
	if db.certificates == nil {
		db.certificates = map[string][]byte{}
	}
	switch args[0] {
	case "-N":
		if err := os.MkdirAll(database, 0700); err != nil {
			return nil, err
		}
		for _, name := range []string{"cert9.db", "key4.db", "pkcs11.txt"} {
			if err := os.WriteFile(filepath.Join(database, name), []byte("fixture"), 0600); err != nil {
				return nil, err
			}
		}
		return nil, nil
	case "-L":
		if nickname != "" {
			certificate := db.certificates[database+"\x00"+nickname]
			if len(certificate) == 0 {
				return nil, errors.New("certificate not found")
			}
			return certificate, nil
		}
		var listing strings.Builder
		for key := range db.certificates {
			if strings.HasPrefix(key, database+"\x00") {
				fmt.Fprintln(&listing, strings.TrimPrefix(key, database+"\x00"), "C,,")
			}
		}
		return []byte(listing.String()), nil
	case "-A":
		certificate, err := os.ReadFile(input)
		if err != nil {
			return nil, err
		}
		block, _ := pem.Decode(certificate)
		if block == nil {
			return nil, errors.New("invalid imported certificate")
		}
		db.certificates[database+"\x00"+nickname] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes})
		return nil, nil
	case "-D":
		delete(db.certificates, database+"\x00"+nickname)
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected certutil operation %q", args[0])
	}
}

func TestLinuxUserTrustInstallsAndRemovesExactOwnedRoot(t *testing.T) {
	_, _, rootPEM := renewalFixture(t, 365)
	root, err := parseLocalTrustRoot(rootPEM)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	profile := filepath.Join(home, ".mozilla", "firefox", "profile.default")
	if err = os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	uid := uint32(os.Geteuid())
	database := linuxNSSDatabase{Path: profile, Create: true}
	fake := &fakeNSSDatabase{}
	if err = installLinuxUserTrustDatabases(t.Context(), home, uid, root, rootPEM, []linuxNSSDatabase{database}, fake.run); err != nil {
		t.Fatal(err)
	}
	present, err := nssLocalRootPresent(t.Context(), profile, root, fake.run)
	if err != nil || !present {
		t.Fatalf("root not installed: present=%v err=%v", present, err)
	}

	foreignPEM := []byte("foreign certificate bytes")
	foreignName := "Foreign Root"
	fake.certificates[profile+"\x00"+foreignName] = foreignPEM
	if err = removeLinuxUserTrustReceipt(t.Context(), root, fake.run); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.certificates[profile+"\x00"+localTrustNickname(root)]; ok {
		t.Fatal("owned root remains in NSS database after cleanup")
	}
	if !bytes.Equal(fake.certificates[profile+"\x00"+foreignName], foreignPEM) {
		t.Fatal("unrelated NSS certificate was removed")
	}
}

func TestLinuxFirefoxProfileIndexAllowsOrdinaryReadPermissions(t *testing.T) {
	home := t.TempDir()
	uid := uint32(os.Geteuid())
	profileRoot := filepath.Join(home, ".mozilla", "firefox")
	profile := filepath.Join(profileRoot, "Profiles", "default-release")
	if err := os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(profileRoot, "profiles.ini")
	index := []byte("[Profile0]\nName=default-release\nIsRelative=1\nPath=Profiles/default-release\n")
	if err := os.WriteFile(indexPath, index, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(indexPath, 0644); err != nil {
		t.Fatal(err)
	}

	var databases []linuxNSSDatabase
	if err := addLinuxFirefoxProfiles(&databases, make(map[string]bool), profileRoot, home, uid); err != nil {
		t.Fatalf("ordinary Firefox profiles.ini was rejected: %v", err)
	}
	if len(databases) != 1 || databases[0].Path != profile || !databases[0].Create {
		t.Fatalf("Firefox profile discovery = %#v, want one creatable database at %q", databases, profile)
	}

	if err := os.Chmod(indexPath, 0664); err != nil {
		t.Fatal(err)
	}
	databases = nil
	if err := addLinuxFirefoxProfiles(&databases, make(map[string]bool), profileRoot, home, uid); err == nil || !strings.Contains(err.Error(), "unsafe Firefox profile index") {
		t.Fatalf("group-writable Firefox profiles.ini was accepted: %v", err)
	}
}

func TestLinuxUserTrustRefusesCorruptReceiptAndNicknameConflict(t *testing.T) {
	_, _, rootPEM := renewalFixture(t, 365)
	root, err := parseLocalTrustRoot(rootPEM)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	uid := uint32(os.Geteuid())
	stateDirectory := filepath.Join(home, ".config", "paperboat", "machineguard", "trusted-ca")
	if err = os.MkdirAll(stateDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(stateDirectory, localTrustFingerprint(root)+".json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(home, "firefox-profile")
	if err = os.Mkdir(profile, 0700); err != nil {
		t.Fatal(err)
	}
	fake := &fakeNSSDatabase{}
	if err = installLinuxUserTrustDatabases(t.Context(), home, uid, root, rootPEM, []linuxNSSDatabase{{Path: profile, Create: true}}, fake.run); err == nil || !strings.Contains(err.Error(), "invalid Paperboat browser trust receipt") {
		t.Fatalf("corrupt receipt accepted: %v", err)
	}
	if _, err = os.Stat(filepath.Join(stateDirectory, localTrustFingerprint(root)+".json")); err != nil {
		t.Fatal("corrupt receipt was not preserved")
	}
	if err = os.Remove(filepath.Join(stateDirectory, localTrustFingerprint(root)+".json")); err != nil {
		t.Fatal(err)
	}

	if err = os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cert9.db", "key4.db", "pkcs11.txt"} {
		if err = os.WriteFile(filepath.Join(profile, name), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	foreignRootPEM := []byte("not the Paperboat root")
	fake.certificates = map[string][]byte{profile + "\x00" + localTrustNickname(root): foreignRootPEM}
	if err = installLinuxUserTrustDatabases(t.Context(), home, uid, root, rootPEM, []linuxNSSDatabase{{Path: profile}}, fake.run); err == nil || !strings.Contains(err.Error(), "occupied by foreign material") {
		t.Fatalf("foreign nickname material accepted: %v", err)
	}
	if !bytes.Equal(fake.certificates[profile+"\x00"+localTrustNickname(root)], foreignRootPEM) {
		t.Fatal("foreign NSS entry was changed")
	}
}

func TestLinuxUserTrustRetiresPriorReceiptsAndKeepsActiveRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("user-scoped browser trust test must not run as root")
	}
	_, _, oldPEM := renewalFixture(t, 365)
	oldRoot, err := parseLocalTrustRoot(oldPEM)
	if err != nil {
		t.Fatal(err)
	}
	_, _, activePEM := renewalFixture(t, 365)
	activeRoot, err := parseLocalTrustRoot(activePEM)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	profile := filepath.Join(home, "firefox-profile")
	if err = os.Mkdir(profile, 0700); err != nil {
		t.Fatal(err)
	}
	uid := uint32(os.Geteuid())
	database := []linuxNSSDatabase{{Path: profile, Create: true}}
	fake := &fakeNSSDatabase{}
	if err = installLinuxUserTrustDatabases(t.Context(), home, uid, oldRoot, oldPEM, database, fake.run); err != nil {
		t.Fatal(err)
	}
	if err = cleanupLinuxUserTrustExcept(t.Context(), oldRoot, fake.run); err != nil {
		t.Fatal(err)
	}
	present, err := nssLocalRootPresent(t.Context(), profile, oldRoot, fake.run)
	if err != nil || !present {
		t.Fatalf("active user root was removed: present=%v err=%v", present, err)
	}
	if err = cleanupLinuxUserTrustExcept(t.Context(), activeRoot, fake.run); err != nil {
		t.Fatal(err)
	}
	present, err = nssLocalRootPresent(t.Context(), profile, oldRoot, fake.run)
	if err != nil || present {
		t.Fatalf("retired user root remains: present=%v err=%v", present, err)
	}
	if err = installLinuxUserTrustDatabases(t.Context(), home, uid, activeRoot, activePEM, database, fake.run); err != nil {
		t.Fatal(err)
	}
	present, err = nssLocalRootPresent(t.Context(), profile, activeRoot, fake.run)
	if err != nil || !present {
		t.Fatalf("new user root not installed: present=%v err=%v", present, err)
	}
}

func TestLinuxNSSCertificatePresenceRequiresSSLTrust(t *testing.T) {
	_, _, rootPEM := renewalFixture(t, 365)
	root, err := parseLocalTrustRoot(rootPEM)
	if err != nil {
		t.Fatal(err)
	}
	nickname := localTrustNickname(root)
	present, err := nssLocalRootTrusted(t.Context(), "fixture", root, func(_ context.Context, args ...string) ([]byte, error) {
		for _, arg := range args {
			if arg == "-a" {
				return rootPEM, nil
			}
		}
		return []byte(nickname + " c,,\n"), nil
	})
	if present || err == nil {
		t.Fatal("untrusted certificate reported trusted")
	}
}

func TestNSSOutputLimit(t *testing.T) {
	out := &boundedNSSOutput{}
	if _, err := out.Write(make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	if _, err := out.Write([]byte("excess")); err == nil || !out.exceeded || out.bytes.Len() != 1<<20 {
		t.Fatal("NSS output was not bounded")
	}
}
