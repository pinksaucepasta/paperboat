package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
	"github.com/pinksaucepasta/paperboat/internal/prompt"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestConfigCredentialInteractiveExplicitFileValidation(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "reference")
	if err := os.WriteFile(file, []byte("reference"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateCredentialInteractiveFile(file); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"relative/key", root, filepath.Join(root, "missing"), root + string(os.PathSeparator) + "directory" + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "reference"} {
		if err := validateCredentialInteractiveFile(path); err == nil {
			t.Fatal("unsafe or unavailable file accepted")
		}
	}
	link := filepath.Join(root, "symlink")
	if err := os.Symlink(file, link); err == nil {
		if err := validateCredentialInteractiveFile(link); err == nil {
			t.Fatal("symlink credential reference accepted")
		}
	}
	if runtime.GOOS != "windows" {
		os.Chmod(file, 0666)
		if err := validateCredentialInteractiveFile(file); err == nil {
			t.Fatal("reference writable by others accepted")
		}
	}
}
func TestConfigCredentialInteractiveSSHKeyAndPassphraseValidation(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	passphrase := "must-never-appear-in-errors"
	block, err := ssh.MarshalPrivateKeyWithPassphrase(key, "", []byte(passphrase))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	keyFile := filepath.Join(root, "key")
	hostsFile := filepath.Join(root, "known_hosts")
	os.WriteFile(keyFile, pem.EncodeToMemory(block), 0600)
	host, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(hostsFile, []byte(knownhosts.Line([]string{"git.example.test"}, host)+"\n"), 0600)
	profile := configsync.RepositoryCredentialProfile{Transport: "ssh", Auth: "ssh", SSHKeyFile: keyFile, SSHKeyPassphrase: passphrase, KnownHostsFile: hostsFile}
	if err := validateCredentialInteractiveProfileReferences(profile); err != nil {
		t.Fatal(err)
	}
	profile.SSHKeyPassphrase = "wrong-private-passphrase"
	err = validateCredentialInteractiveProfileReferences(profile)
	if err == nil || strings.Contains(err.Error(), profile.SSHKeyPassphrase) || strings.Contains(err.Error(), passphrase) {
		t.Fatal("invalid passphrase not safely rejected")
	}
	profile.SSHKeyPassphrase = passphrase
	os.WriteFile(hostsFile, []byte("not a known hosts entry\n"), 0600)
	if err := validateCredentialInteractiveProfileReferences(profile); err == nil {
		t.Fatal("invalid known_hosts accepted")
	}
}
func TestConfigCredentialInteractiveCACertificateValidation(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, public, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600)
	profile := configsync.RepositoryCredentialProfile{Transport: "https", Auth: "anonymous", CAFile: path}
	if err := validateCredentialInteractiveProfileReferences(profile); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte("not a certificate"), 0600)
	if err := validateCredentialInteractiveProfileReferences(profile); err == nil {
		t.Fatal("invalid CA bundle accepted")
	}
}
func TestConfigCredentialInteractiveSecretRejectsNonTerminal(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetIn(input)
	command.SetErr(&output)
	if _, err := credentialInteractiveSecret(command, "Secret", "Hidden input", false); !errors.Is(err, prompt.ErrNotTerminal) {
		t.Fatal("plaintext input accepted", err)
	}
	if output.Len() != 0 {
		t.Fatal("nonterminal secret prompt emitted output")
	}
}
