package managedssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func TestServerServesBoundedAgentAndCancels(t *testing.T) {
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(private)
	managed, _ := NewAgent(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (Server{Agent: managed, MaxConnections: 2, IdleTimeout: time.Second}).Serve(ctx, listener)
	}()
	connection, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client := agent.NewClient(connection)
	keys, err := client.List()
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys=%+v error=%v", keys, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServerRejectsOversizedFrame(t *testing.T) {
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(private)
	managed, _ := NewAgent(signer)
	server, client := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- (Server{Agent: managed, MaxConnections: 1, IdleTimeout: time.Second}).serveConnection(context.Background(), server)
	}()
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], MaxAgentRequestBytes+1)
	if _, err := client.Write(header[:]); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != ErrAgentRequestTooLarge {
		t.Fatalf("oversized frame error=%v", err)
	}
	_ = client.Close()
}

type failingAcceptListener struct{ err error }

func (l failingAcceptListener) Accept() (net.Conn, error) { return nil, l.err }
func (failingAcceptListener) Close() error                { return nil }
func (failingAcceptListener) Addr() net.Addr              { return &net.TCPAddr{} }

func TestServerClassifiesTerminalListenerAcceptFailure(t *testing.T) {
	signer, _ := testKey(t)
	managed, err := NewAgent(signer)
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("private socket path /home/user/.ssh/agent.sock")
	err = (Server{Agent: managed, MaxConnections: 1, IdleTimeout: time.Second}).Serve(context.Background(), failingAcceptListener{err: cause})
	if !errors.Is(err, cause) || err.Error() != errManagedSSHOperation.Error() {
		t.Fatalf("listener failure text/cause mismatch: %v", err)
	}
	var classified interface {
		DiagnosticStage() string
		DiagnosticCode() string
	}
	if !errors.As(err, &classified) || classified.DiagnosticStage() != "listener_accept" || classified.DiagnosticCode() != "managed_ssh_failed" {
		t.Fatalf("listener failure classification missing: %T %v", err, err)
	}
}

func TestListenOwnerSocketEnforcesPathOwnership(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("Unix-only socket test")
	}
	directory, err := os.MkdirTemp("/tmp", "pb-ssh-agent-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "agent.sock")
	listener, err := ListenOwnerSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket info=%+v error=%v", info, err)
	}
	if _, err := ListenOwnerSocket(path); err == nil {
		t.Fatal("existing socket was replaced")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("socket remains after close: %v", err)
	}
	unsafe := filepath.Join(directory, "unsafe")
	if err := os.Mkdir(unsafe, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ListenOwnerSocket(filepath.Join(unsafe, "agent.sock")); err == nil {
		t.Fatal("permissive runtime directory was accepted")
	}
}

func TestListenOwnerSocketReclaimsVerifiedStaleSocket(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("Unix-domain stale socket test")
	}
	directory, err := os.MkdirTemp("/tmp", "pb-ssh-agent-stale-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "agent.sock")
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	listener, err := ListenOwnerSocket(path)
	if err != nil {
		t.Fatalf("reclaim stale socket: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
}
