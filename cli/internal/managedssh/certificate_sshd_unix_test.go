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
	"strings"
	"sync"
	"testing"
	"time"

	wire "github.com/pinksaucepasta/paperboat/shared/managedssh"
	"golang.org/x/crypto/ssh"
)

// The host lines ReconcileAuthorizedKeys writes, checked by the machine's real sshd: a certificate logs in only when
// its CA is trusted, it is valid now, it carries the machine's principal and it comes from loopback (the bridge: a
// login from any other address is refused, so the bridge's revocation check cannot be bypassed); a KRL naming user A's
// key ID (RevokedKeys) refuses A while B's certificate from the same CA keeps working; a rotation keeps both CAs'
// certificates working.
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
	external := nonLoopbackAddress()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	configPath := filepath.Join(root, "sshd_config")
	krlPath := filepath.Join(root, "revoked.krl")
	writeKRL := func(cas []ssh.Signer, keyIDs ...string) {
		t.Helper()
		blobs := make([][]byte, len(cas))
		for index, ca := range cas {
			blobs[index] = ca.PublicKey().Marshal()
		}
		if err := os.WriteFile(krlPath, wire.KRL(blobs, keyIDs, uint64(time.Now().UnixNano()), uint64(time.Now().Unix())), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeKRL(nil)
	listen := "ListenAddress 127.0.0.1\n"
	if external != "" {
		listen += "ListenAddress " + external + "\n"
	}
	config := fmt.Sprintf(listen+"RevokedKeys "+krlPath+"\nPort %d\nHostKey %s\nPidFile %s\nAuthorizedKeysFile %s\nStrictModes no\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nUsePAM no\nAllowUsers %s\nLogLevel VERBOSE\n",
		port, hostKeyPath, filepath.Join(root, "sshd.pid"), filepath.Join(home, ".ssh", "authorized_keys"), current.Username)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var sshdLog lockedBuffer
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
	var loginFrom func(certificate, address string) error
	login := func(certificate string) error { return loginFrom(certificate, "127.0.0.1") }
	loginFrom = func(certificate, address string) error {
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
			current.Username+"@"+address, "echo managed-cert-ok")
		output, err := command.CombinedOutput()
		if err == nil && !bytes.Contains(output, []byte("managed-cert-ok")) {
			err = fmt.Errorf("unexpected output %q", output)
		}
		return err
	}
	now := time.Now()
	const principal, keyA, keyB = "pb:machine_1", "paperboat:user_a:session_a:machine_1", "paperboat:user_b:session_b:machine_1"
	user := current.Username
	issue := func(ca ssh.Signer, keyID string, principals []string, issued time.Time) string {
		return sshdTestCertificate(t, ca, clientKey.PublicKey(), keyID, principals, issued)
	}
	valid := issue(caOne, keyA, []string{user, principal}, now)
	trust(principal, caOne)
	if err := login(valid); err != nil {
		t.Fatalf("valid certificate refused: %v\n%s", err, sshdLog.String())
	}
	refused := map[string]string{
		"expired":         issue(caOne, keyA, []string{user, principal}, now.Add(-13*time.Hour)),
		"wrong principal": issue(caOne, keyA, []string{user, "pb:machine_2"}, now),
		"wrong CA":        issue(caTwo, keyA, []string{user, principal}, now),
	}
	for name, certificate := range refused {
		if err := login(certificate); err == nil {
			t.Errorf("%s certificate logged in", name)
		}
	}
	if external != "" {
		if err := loginFrom(valid, external); err == nil {
			t.Fatal("a managed certificate logged in from a non-loopback address, bypassing the bridge")
		}
	} else {
		t.Log("no non-loopback address on this machine: the bridge-bypass login is not exercised")
	}
	// Revoking user A (the KRL sshd reads) refuses A's certificate; B's, from the same CA, keeps logging in.
	certificateB := issue(caOne, keyB, []string{user, principal}, now)
	writeKRL([]ssh.Signer{caOne}, keyA)
	if err := login(valid); err == nil {
		t.Fatal("user A's revoked certificate logged in")
	}
	if err := login(certificateB); err != nil {
		t.Fatalf("user B's certificate stopped working when A was revoked: %v\n%s", err, sshdLog.String())
	}
	if !strings.Contains(sshdLog.String(), "revoked by file") {
		t.Fatalf("sshd did not refuse A by the KRL:\n%s", sshdLog.String())
	}
	writeKRL(nil)
	// Rotation: both CAs are trusted while the new one is pending/active, then only the new one.
	trust(principal, caOne, caTwo)
	for name, ca := range map[string]ssh.Signer{"old CA": caOne, "new CA": caTwo} {
		if err := login(issue(ca, keyB, []string{user, principal}, now)); err != nil {
			t.Fatalf("%s certificate refused during rotation: %v", name, err)
		}
	}
	trust(principal, caTwo)
	if err := login(issue(caOne, keyB, []string{user, principal}, now)); err == nil {
		t.Fatal("the retired CA's certificate logged in")
	}
}

// lockedBuffer is sshd's stderr, read by the test while sshd still writes it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *lockedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.String() }

// sshdTestCertificate is a certificate as the server issues it: key ID, principals, 12 h, bound to loopback.
func sshdTestCertificate(t *testing.T, ca ssh.Signer, key ssh.PublicKey, keyID string, principals []string, issued time.Time) string {
	t.Helper()
	certificate := &ssh.Certificate{Key: key, CertType: ssh.UserCert, KeyId: keyID, ValidPrincipals: principals,
		ValidAfter: uint64(issued.Add(-time.Minute).Unix()), ValidBefore: uint64(issued.Add(12 * time.Hour).Unix()),
		Permissions: ssh.Permissions{CriticalOptions: map[string]string{"source-address": "127.0.0.1/32,::1/128"}, Extensions: map[string]string{"permit-pty": ""}}}
	if err := certificate.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(certificate)))
}

// nonLoopbackAddress is one IPv4 address of this machine other than loopback, or "".
func nonLoopbackAddress() string {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, address := range addresses {
		if network, ok := address.(*net.IPNet); ok && network.IP.To4() != nil && !network.IP.IsLoopback() {
			return network.IP.String()
		}
	}
	return ""
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
