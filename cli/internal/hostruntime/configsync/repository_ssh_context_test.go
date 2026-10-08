package configsync

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestRepositorySSHBlockedOperationCancellation(t *testing.T) {
	for _, phase := range []string{"handshake", "references", "pack", "handshake_deadline", "references_deadline", "pack_deadline"} {
		t.Run(phase, func(t *testing.T) {
			deadline := strings.HasSuffix(phase, "_deadline")
			phase = strings.TrimSuffix(phase, "_deadline")
			host, _ := sshFixtureKey(t)
			identity, key := sshFixtureKey(t)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			entered := make(chan struct{})
			joined := make(chan struct{})
			go func() {
				defer close(joined)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				if phase == "handshake" {
					close(entered)
					_, _ = io.Copy(io.Discard, conn)
					return
				}
				cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
					if string(k.Marshal()) != string(identity.PublicKey().Marshal()) {
						return nil, ErrAuthorization
					}
					return &ssh.Permissions{}, nil
				}}
				cfg.AddHostKey(host)
				server, channels, requests, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				incoming := <-channels
				channel, requests, err := incoming.Accept()
				if err != nil {
					return
				}
				defer channel.Close()
				req := <-requests
				if req.Type != "exec" {
					return
				}
				_ = req.Reply(true, nil)
				if phase == "pack" {
					refs := packp.NewAdvRefs()
					hash := plumbing.NewHash("0123456789012345678901234567890123456789")
					refs.Head = &hash
					refs.References["refs/heads/main"] = hash
					if refs.Encode(channel) != nil {
						return
					}
					_, _ = io.Copy(io.Discard, channel)
					_, _ = channel.Write([]byte("0008NAK\n"))
				}
				close(entered)
				_ = server.Wait()
			}()
			root := filepath.Join(t.TempDir(), "private")
			if err := EnsureRepositoryCredentialRoot(root); err != nil {
				t.Fatal(err)
			}
			keyFile, hosts := filepath.Join(root, "identity"), filepath.Join(root, "known_hosts")
			if err := writePrivateAtomic(keyFile, key); err != nil {
				t.Fatal(err)
			}
			if err := writePrivateAtomic(hosts, []byte(knownhosts.Line([]string{listener.Addr().String()}, host.PublicKey())+"\n")); err != nil {
				t.Fatal(err)
			}
			if err := SaveRepositoryCredentialProfile(root, "repo", RepositoryCredentialProfile{Transport: "ssh", Auth: "ssh", SSHKeyFile: keyFile, KnownHostsFile: hosts}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
			}
			defer cancel()
			access, err := resolveRepositoryTransportAccess(ctx, root, RepositoryAccess{RepositoryID: "repo", Transport: "ssh", CloneURL: "ssh://git@" + listener.Addr().String() + "/repo.git"})
			if err != nil {
				t.Fatal(err)
			}
			defer access.close()
			done := make(chan error, 1)
			go func() {
				ep, _ := transport.NewEndpoint("ssh://git@" + listener.Addr().String() + "/repo.git")
				session, err := newRepositorySSHSession(ep, access.auth, transport.UploadPackServiceName)
				if err != nil {
					done <- err
					return
				}
				defer session.Close()
				refs, err := session.AdvertisedReferencesContext(ctx)
				if err != nil {
					done <- err
					return
				}
				request := packp.NewUploadPackRequest()
				request.Wants = []plumbing.Hash{*refs.Head}
				response, err := session.UploadPack(ctx, request)
				if err == nil {
					_, err = io.Copy(io.Discard, response)
					_ = response.Close()
				}
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("fixture did not reach blocked phase")
			}
			if !deadline {
				cancel()
			}
			select {
			case err := <-done:
				expected := context.Canceled
				if deadline {
					expected = context.DeadlineExceeded
				}
				if !errors.Is(err, expected) {
					t.Fatalf("blocked %s cancellation = %v", phase, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("operation ignored cancellation")
			}
			select {
			case <-joined:
			case <-time.After(time.Second):
				t.Fatal("SSH fixture connection retained")
			}
		})
	}
}
