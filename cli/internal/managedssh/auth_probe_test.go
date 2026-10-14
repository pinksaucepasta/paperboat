package managedssh

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestProbeSSHAuthenticationProvesManagedKeyAndHostPinWithoutChannel(t *testing.T) {
	clientSigner, _ := testKey(t)
	hostSigner, _ := testKey(t)
	serverConfig := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if string(key.Marshal()) != string(clientSigner.PublicKey().Marshal()) {
			return nil, errors.New("wrong client key")
		}
		return nil, nil
	}}
	serverConfig.AddHostKey(hostSigner)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan error, 1)
	go func() {
		server, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		connection, channels, _, err := ssh.NewServerConn(server, serverConfig)
		if err == nil {
			select {
			case channel := <-channels:
				if channel != nil {
					err = errors.New("authentication probe opened a channel")
				}
			case <-time.After(50 * time.Millisecond):
			}
			_ = connection.Close()
		}
		serverDone <- err
	}()
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	public := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostSigner.PublicKey())))
	if err := ProbeSSHAuthentication(t.Context(), client, "studio.local.pprbt.dev:22", "deploy", clientSigner, []string{public}); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestProbeSSHAuthenticationHonorsCancellation(t *testing.T) {
	clientSigner, _ := testKey(t)
	client, server := net.Pipe()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := ProbeSSHAuthentication(ctx, client, "studio.local.pprbt.dev:22", "deploy", clientSigner, []string{strings.TrimSpace(string(ssh.MarshalAuthorizedKey(clientSigner.PublicKey())))})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error=%v", err)
	}
}

type failingAuthenticationStream struct{ cause error }

func (failingAuthenticationStream) Write(value []byte) (int, error) { return len(value), nil }
func (s failingAuthenticationStream) Read([]byte) (int, error)      { return 0, s.cause }
func (failingAuthenticationStream) Close() error                    { return nil }

func TestProbeSSHAuthenticationKeepsTransportCauseWithStaticText(t *testing.T) {
	clientSigner, _ := testKey(t)
	cause := errors.New("private host address and handshake detail")
	err := ProbeSSHAuthentication(t.Context(), failingAuthenticationStream{cause: cause}, "studio.local.pprbt.dev:22", "deploy", clientSigner, []string{"not a host key"})
	if !errors.Is(err, ErrSSHAuthentication) {
		t.Fatalf("authentication classification lost: %v", err)
	}
	if !errors.Is(err, ErrHostKeyInventory) {
		t.Fatalf("host-key validation cause lost: %v", err)
	}

	valid := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(clientSigner.PublicKey())))
	err = ProbeSSHAuthentication(t.Context(), failingAuthenticationStream{cause: cause}, "studio.local.pprbt.dev:22", "deploy", clientSigner, []string{valid})
	if !errors.Is(err, cause) {
		t.Fatalf("transport cause lost: %v", err)
	}
	if err.Error() != ErrSSHAuthentication.Error() || strings.Contains(err.Error(), "private host address") {
		t.Fatalf("authentication failure exposed transport text: %q", err)
	}
}
