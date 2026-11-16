//go:build darwin || linux

package managedssh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// The host lines ReconcileAuthorizedKeys writes, checked by the machine's real sshd: a certificate logs in only when
// its CA is trusted, it is valid now and it carries the machine's current revocation principal; changing the
// principal (a revocation) refuses every earlier certificate; a rotation keeps both CAs' certificates working.
func TestRealSSHDHonoursTheManagedCALines(t *testing.T) {
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH client is unavailable")
	}
	sshdPath, err := exec.LookPath("sshd")
	if err != nil {
		if _, statErr := os.Stat("/usr/sbin/sshd"); statErr != nil {
			t.Skip("sshd is unavailable")
		}
		sshdPath = "/usr/sbin/sshd"
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	home := managedSSHTestHome(t)
	root := shortRuntimeDirectory(t, "pb-sshd-")
	hostKeyPath, clientKeyPath := filepath.Join(root, "host"), filepath.Join(root, "client")
	writeTestPrivateKey(t, hostKeyPath)
	clientKey := writeTestPrivateKey(t, clientKeyPath)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	configPath := filepath.Join(root, "sshd_config")
	config := fmt.Sprintf("ListenAddress 127.0.0.1\nPort %d\nHostKey %s\nPidFile %s\nAuthorizedKeysFile %s\nStrictModes no\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nUsePAM no\nAllowUsers %s\nLogLevel VERBOSE\n",
		port, hostKeyPath, filepath.Join(root, "sshd.pid"), filepath.Join(home, ".ssh", "authorized_keys"), current.Username)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var sshdLog bytes.Buffer
	sshd := exec.CommandContext(ctx, sshdPath, "-D", "-e", "-f", configPath)
	sshd.Stderr = &sshdLog
	if err := sshd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = sshd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 200*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sshd did not start: %s", sshdLog.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	caOne, caTwo := testSignerKey(t), testSignerKey(t)
	caLine := func(ca ssh.Signer) string { return string(bytes.TrimSpace(ssh.MarshalAuthorizedKey(ca.PublicKey()))) }
	trust := func(principal string, cas ...ssh.Signer) {
		t.Helper()
		lines := make([]string, len(cas))
		for index, ca := range cas {
			lines[index] = caLine(ca)
		}
		if _, err := ReconcileAuthorizedKeys(home, uint32(os.Getuid()), lines, principal); err != nil {
			t.Fatal(err)
		}
	}
	login := func(certificate string) error {
		t.Helper()
		certificatePath := filepath.Join(root, "client-cert.pub")
		if err := os.WriteFile(certificatePath, []byte(certificate+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runCtx, stop := context.WithTimeout(t.Context(), 10*time.Second)
		defer stop()
		command := exec.CommandContext(runCtx, sshPath, "-F", "/dev/null", "-p", strconv.Itoa(port),
			"-o", "IdentityFile="+clientKeyPath, "-o", "CertificateFile="+certificatePath, "-o", "IdentitiesOnly=yes",
			"-o", "IdentityAgent=none", "-o", "BatchMode=yes", "-o", "PreferredAuthentications=publickey",
			"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR",
			current.Username+"@127.0.0.1", "echo managed-cert-ok")
		output, err := command.CombinedOutput()
		if err == nil && !bytes.Contains(output, []byte("managed-cert-ok")) {
			err = fmt.Errorf("unexpected output %q", output)
		}
		return err
	}
	now := time.Now()
	const principal, revoked = "pb:machine_1:0", "pb:machine_1:1a2b3c4d5e6f7a8b"
	user := current.Username
	valid := testCertificate(t, caOne, clientKey.PublicKey(), []string{user, principal}, now, 12*time.Hour)
	trust(principal, caOne)
	if err := login(valid); err != nil {
		t.Fatalf("valid certificate refused: %v\n%s", err, sshdLog.String())
	}
	refused := map[string]string{
		"expired":         testCertificate(t, caOne, clientKey.PublicKey(), []string{user, principal}, now.Add(-13*time.Hour), 12*time.Hour),
		"wrong principal": testCertificate(t, caOne, clientKey.PublicKey(), []string{user, "pb:machine_2:0"}, now, 12*time.Hour),
		"wrong CA":        testCertificate(t, caTwo, clientKey.PublicKey(), []string{user, principal}, now, 12*time.Hour),
	}
	for name, certificate := range refused {
		if err := login(certificate); err == nil {
			t.Errorf("%s certificate logged in", name)
		}
	}
	// A revocation push changes the principal: the earlier certificate is refused, a new one logs in.
	trust(revoked, caOne)
	if err := login(valid); err == nil {
		t.Fatal("a certificate issued before the revocation logged in")
	}
	if err := login(testCertificate(t, caOne, clientKey.PublicKey(), []string{user, revoked}, now, 12*time.Hour)); err != nil {
		t.Fatalf("a certificate issued after the revocation was refused: %v", err)
	}
	// Rotation: both CAs are trusted while the new one is pending/active, then only the new one.
	trust(revoked, caOne, caTwo)
	for name, ca := range map[string]ssh.Signer{"old CA": caOne, "new CA": caTwo} {
		if err := login(testCertificate(t, ca, clientKey.PublicKey(), []string{user, revoked}, now, 12*time.Hour)); err != nil {
			t.Fatalf("%s certificate refused during rotation: %v", name, err)
		}
	}
	trust(revoked, caTwo)
	if err := login(testCertificate(t, caOne, clientKey.PublicKey(), []string{user, revoked}, now, 12*time.Hour)); err == nil {
		t.Fatal("the retired CA's certificate logged in")
	}
}

// writeTestPrivateKey writes a new ed25519 private key file for OpenSSH and returns its signer.
func writeTestPrivateKey(t *testing.T, path string) ssh.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}
