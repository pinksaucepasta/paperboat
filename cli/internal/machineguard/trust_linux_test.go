//go:build linux

package machineguard

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
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

	foreignPath := linuxOwnedTrustPath(trust, testBrowserDomain)
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
	trust        map[string]string // per certificate key; absent means "C,," (what -A -t C,, sets)
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
				trust := "C,,"
				if flags, ok := db.trust[key]; ok {
					trust = flags
				}
				fmt.Fprintln(&listing, strings.TrimPrefix(key, database+"\x00"), trust)
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
	case "-M":
		if _, ok := db.certificates[database+"\x00"+nickname]; !ok {
			return nil, errors.New("certificate not found")
		}
		if db.trust == nil {
			db.trust = map[string]string{}
		}
		db.trust[database+"\x00"+nickname] = args[len(args)-1]
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected certutil operation %q", args[0])
	}
}

func TestLinuxUserTrustInstallsAndRemovesExactOwnedRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("browser trust cleanup refuses root by design; this runs in the unprivileged test step")
	}
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

// root's browser trust is the system store the machine guard installs: as root every user-trust entry point succeeds
// without an NSS command and leaves the home directory untouched (the unprivileged test above covers the user path).
func TestLinuxUserTrustIsSystemTrustForRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root; runs in tools/cli-root-tests.sh")
	}
	_, _, rootPEM := renewalFixture(t, 365)
	root, err := parseLocalTrustRoot(rootPEM)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if err = os.MkdirAll(filepath.Join(home, ".mozilla", "firefox", "profile.default"), 0700); err != nil {
		t.Fatal(err)
	}
	before := linuxTrustTree(t, home)
	commands := 0
	run := func(context.Context, ...string) ([]byte, error) { commands++; return nil, nil }
	for name, call := range map[string]func() error{
		"install":        func() error { return installLinuxUserTrust(t.Context(), rootPEM, run) },
		"cleanup":        func() error { return cleanupLinuxUserTrust(t.Context(), run) },
		"cleanup except": func() error { return cleanupLinuxUserTrustExcept(t.Context(), root, run) },
		"remove receipt": func() error { return removeLinuxUserTrustReceipt(t.Context(), root, run) },
	} {
		if err := call(); err != nil {
			t.Fatalf("%s as root: %v", name, err)
		}
	}
	if commands != 0 || linuxTrustTree(t, home) != before {
		t.Fatalf("root user trust touched things: %d NSS commands, tree changed=%v", commands, linuxTrustTree(t, home) != before)
	}
}

func linuxTrustTree(t *testing.T, home string) string {
	t.Helper()
	var paths []string
	if err := filepath.WalkDir(home, func(path string, _ fs.DirEntry, err error) error {
		paths = append(paths, path)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return strings.Join(paths, "\n")
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

// A browser database that returns several certificates for one nickname still holds our root.
func TestLinuxNSSRootPresentWhenTheNicknameListsSeveralCertificates(t *testing.T) {
	_, _, otherPEM := renewalFixture(t, 365)
	_, _, rootPEM := renewalFixture(t, 365)
	root, err := parseLocalTrustRoot(rootPEM)
	if err != nil {
		t.Fatal(err)
	}
	nickname := localTrustNickname(root)
	present, err := nssLocalRootPresent(t.Context(), "fixture", root, func(_ context.Context, args ...string) ([]byte, error) {
		for _, arg := range args {
			if arg == "-a" {
				return append(append([]byte(nil), otherPEM...), rootPEM...), nil
			}
		}
		return []byte(nickname + " C,,\n"), nil
	})
	if err != nil || !present {
		t.Fatalf("root among several certificates: present=%v err=%v", present, err)
	}
}

// A retired Paperboat root whose browser entry cannot be retired safely must not stop the current
// root from being installed: the entry is left alone and the active root is trusted.
func TestLinuxUserTrustInstallsTheActiveRootDespiteAnUnretirableOldEntry(t *testing.T) {
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
	t.Setenv("XDG_DATA_HOME", "")
	bin := t.TempDir()
	if err = os.WriteFile(filepath.Join(bin, "certutil"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	profile := filepath.Join(home, ".local", "share", "pki", "nssdb")
	database := []linuxNSSDatabase{{Path: profile, Create: true}}
	fake := &fakeNSSDatabase{}
	uid := uint32(os.Geteuid())
	if err = installLinuxUserTrustDatabases(t.Context(), home, uid, oldRoot, oldPEM, database, fake.run); err != nil {
		t.Fatal(err)
	}
	// The old nickname now holds material that is not the old root (what the device showed).
	_, _, strangerPEM := renewalFixture(t, 365)
	stranger := append(append([]byte(nil), strangerPEM...), strangerPEM...)
	fake.certificates[profile+"\x00"+localTrustNickname(oldRoot)] = stranger

	if err = installLinuxUserTrust(t.Context(), activePEM, fake.run); err != nil {
		t.Fatalf("an old Paperboat entry blocked the active root: %v", err)
	}
	present, err := nssLocalRootPresent(t.Context(), profile, activeRoot, fake.run)
	if err != nil || !present {
		t.Fatalf("active root not installed: present=%v err=%v", present, err)
	}
	if !bytes.Equal(fake.certificates[profile+"\x00"+localTrustNickname(oldRoot)], stranger) {
		t.Fatal("foreign material under an old nickname was changed")
	}
}

// certutil -A writes the certificate and its trust apart: a write killed between them (dadape, 2026-11-26) left
// Paperboat's own root without SSL trust under a pending receipt, and every later pass refused it. The next pass
// finishes that install; a root without trust and without our pending receipt stays preserved.
func TestLinuxUserTrustFinishesAnInstallCutOffBeforeItsTrust(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("browser trust refuses root by design; this runs in the unprivileged test step")
	}
	_, _, rootPEM := renewalFixture(t, 365)
	root, err := parseLocalTrustRoot(rootPEM)
	if err != nil {
		t.Fatal(err)
	}
	for _, owned := range []bool{true, false} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
		profile := filepath.Join(home, ".mozilla", "firefox", "profile.default")
		if err = os.MkdirAll(profile, 0700); err != nil {
			t.Fatal(err)
		}
		uid := uint32(os.Geteuid())
		databases := []linuxNSSDatabase{{Path: profile, Create: true}}
		key := profile + "\x00" + localTrustNickname(root)
		fake := &fakeNSSDatabase{}
		if owned {
			cutOff := func(ctx context.Context, args ...string) ([]byte, error) {
				out, err := fake.run(ctx, args...)
				if err == nil && args[0] == "-A" {
					fake.trust = map[string]string{key: ",,"} // the certificate landed, its trust did not
					return nil, errors.New("signal: killed")
				}
				return out, err
			}
			if err = installLinuxUserTrustDatabases(t.Context(), home, uid, root, rootPEM, databases, cutOff); err == nil {
				t.Fatal("setup: the cut-off install reported success")
			}
		} else {
			_, _ = fake.run(t.Context(), "-N", "-d", "sql:"+profile)
			block, _ := pem.Decode(rootPEM)
			fake.certificates = map[string][]byte{key: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes})}
			fake.trust = map[string]string{key: ",,"}
		}
		err = installLinuxUserTrustDatabases(t.Context(), home, uid, root, rootPEM, databases, fake.run)
		if owned && (err != nil || fake.trust[key] != "C,,") {
			t.Fatalf("our cut-off install was not finished: err=%v trust=%q", err, fake.trust[key])
		}
		if !owned && (!errors.Is(err, errNSSRootWithoutSSLTrust) || fake.trust[key] != ",,") {
			t.Fatalf("a root without trust that we did not start installing was changed: err=%v trust=%q", err, fake.trust[key])
		}
	}
}

// An earlier install whose guard state was deleted (a manual cleanup, a test run)
// leaves its Paperboat root at the same owned trust path. That orphan root is
// replaced, so pb install recovers; any other content is still preserved.
func TestLinuxSystemTrustReplacesOrphanedPaperboatRoot(t *testing.T) {
	_, _, oldPEM := renewalFixture(t, 365)
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
	path := linuxOwnedTrustPath(trust, localTrustDomain(root))
	if err = os.WriteFile(path, oldPEM, 0644); err != nil {
		t.Fatal(err)
	}
	runner := func(context.Context, string, ...string) ([]byte, error) {
		return nil, os.WriteFile(bundle, rootPEM, 0644)
	}
	if err = installLinuxSystemTrustAt(t.Context(), trust, bundle, root, rootPEM, uint32(os.Geteuid()), runner); err != nil {
		t.Fatalf("orphaned Paperboat root blocked install: %v", err)
	}
	if contents, _ := os.ReadFile(path); !bytes.Equal(contents, rootPEM) {
		t.Fatal("orphaned Paperboat root was not replaced")
	}
}
