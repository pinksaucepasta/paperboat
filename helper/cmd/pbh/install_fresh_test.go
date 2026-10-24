package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/helper/internal/containers"
)

// The release installer places the runtime binary before pbh install runs. A
// pbh that runs without it must say which path is missing and write nothing.
func TestServicePackageBinaryNamesMissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "pbh")
	err := servicePackageBinary(missing)
	var owned commandError
	if !errors.As(err, &owned) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing runtime binary: %v", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("missing runtime binary message does not name the path: %v", err)
	}
	if !strings.Contains(err.Error(), "release installer") {
		t.Fatalf("missing runtime binary message omits the remedy: %v", err)
	}
}

func TestDiscardFreshInstallRemovesOnlyInstallationFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "protected")
	if err := discardFreshInstall(dir); err != nil {
		t.Fatalf("absent state directory: %v", err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	installation := []string{"selfhost.json", "images.json", "tls.crt", "tls.key", ".selfhost.lock", "public-https-cert-0123456789abcdef.pem", "public-https-key-0123456789abcdef.pem", "control-ca-0123456789abcdef.pem"}
	for _, name := range append(installation, "operator-notes.txt") {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := discardFreshInstall(dir); err != nil {
		t.Fatalf("discard: %v", err)
	}
	for _, name := range installation {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s survived a failed first install: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "operator-notes.txt")); err != nil {
		t.Fatalf("unrelated file was removed: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "operator-notes.txt")); err != nil {
		t.Fatal(err)
	}
	if err := discardFreshInstall(dir); err != nil {
		t.Fatalf("empty directory: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("empty state directory was kept: %v", err)
	}
}

// A first install whose trust files are refused must leave no state behind,
// so a rerun with corrected files is a first install again.
func TestFreshInstallRefusedAfterStagingLeavesNoState(t *testing.T) {
	if err := containers.CheckHost(); err != nil {
		t.Skip("supported installer host required")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	cert := filepath.Join(source, "fullchain.pem")
	writeTestCertificate(t, cert, 0644)
	notCertificate := filepath.Join(source, "control-ca.pem")
	if err := os.WriteFile(notCertificate, []byte("not a certificate"), 0644); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	claim := listener.Addr().String()
	listener.Close()
	dir := filepath.Join(t.TempDir(), "protected")
	args := []string{"install", "--no-service", "--state-dir", dir, "--endpoint-host", "203.0.113.10", "--claim-listen", claim, "--relay-image", "example.test/relay:next", "--https-cert", cert, "--control-ca", notCertificate}
	if err := run(context.Background(), args, io.Discard); err == nil || !strings.Contains(err.Error(), notCertificate) {
		t.Fatalf("install accepted an invalid control CA: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		entries, _ := os.ReadDir(dir)
		t.Fatalf("refused first install left state behind: %v %v", fmt.Sprint(len(entries)), err)
	}
}
