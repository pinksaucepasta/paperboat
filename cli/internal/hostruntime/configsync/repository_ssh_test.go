package configsync

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

func sshFixtureKey(t *testing.T) (ssh.Signer, []byte) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	return signer, pem.EncodeToMemory(block)
}
func TestCustomSSHNoGitStrictHostKeyAndAuth(t *testing.T) {
	bare, _ := transportFixture(t)
	setSharedSource(t, bare, []byte(`"version" = 1
`))
	host, _ := sshFixtureKey(t)
	client, key := sshFixtureKey(t)
	conf := &ssh.ServerConfig{PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if c.User() != "git" || string(k.Marshal()) != string(client.PublicKey().Marshal()) {
			return nil, ErrAuthorization
		}
		return &ssh.Permissions{}, nil
	}}
	extraKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	extraSigner, err := ssh.NewSignerFromKey(extraKey)
	if err != nil {
		t.Fatal(err)
	}
	conf.AddHostKey(extraSigner)
	conf.AddHostKey(host)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	connections := map[net.Conn]bool{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			connections[conn] = true
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { conn.Close(); mu.Lock(); delete(connections, conn); mu.Unlock() }()
				s, channels, requests, err := ssh.NewServerConn(conn, conf)
				if err != nil {
					return
				}
				defer s.Close()
				go ssh.DiscardRequests(requests)
				for ch := range channels {
					if ch.ChannelType() != "session" {
						ch.Reject(ssh.UnknownChannelType, "session required")
						continue
					}
					stream, reqs, err := ch.Accept()
					if err != nil {
						return
					}
					func() {
						defer stream.Close()
						for req := range reqs {
							if req.Type != "exec" {
								req.Reply(false, nil)
								continue
							}
							var exec struct{ Command string }
							if ssh.Unmarshal(req.Payload, &exec) != nil {
								req.Reply(false, nil)
								return
							}
							service := strings.Fields(exec.Command)
							if len(service) != 2 || service[1] != "'/repo.git'" {
								req.Reply(false, nil)
								return
							}
							req.Reply(true, nil)
							if err := serveSSHGit(stream, bare, service[0]); err != nil {
								t.Logf("SSH protocol %s: %v", service[0], err)
							}
							stream.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
							return
						}
					}()
				}
			}()
		}
	}()
	defer func() {
		ln.Close()
		mu.Lock()
		for c := range connections {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	}()
	root := t.TempDir()
	if err := EnsureRepositoryCredentialRoot(root); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(root, "key")
	hosts := filepath.Join(root, "known_hosts")
	os.WriteFile(keyFile, key, 0600)
	os.WriteFile(hosts, []byte(knownhosts.Line([]string{ln.Addr().String()}, host.PublicKey())+"\n"), 0600)
	p := RepositoryCredentialProfile{Transport: "ssh", Auth: "ssh", SSHKeyFile: keyFile, KnownHostsFile: hosts}
	if err := SaveRepositoryCredentialProfile(root, "repo", p); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")
	repo := newTransportRepository(t, "ssh://git@"+ln.Addr().String()+"/repo.git", "ssh", root)
	assertBootstrapRead(t, "ssh://git@"+ln.Addr().String()+"/repo.git", "ssh", root)
	snap, err := repo.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := repo.Reconcile(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	result, err := repo.Publish(context.Background(), prepared, 1)
	if err != nil || !result.Landed {
		t.Fatalf("SSH publication landed=%v error=%v", result.Landed, err)
	}
	observed, _, err := repo.ObserveCommit(context.Background(), prepared.CommitID)
	if err != nil || !observed {
		t.Fatal("SSH observation failed", err)
	}
	if runtime.GOOS != "windows" {
		ring := agent.NewKeyring()
		raw, err := ssh.ParseRawPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		if err := ring.Add(agent.AddedKey{PrivateKey: raw}); err != nil {
			t.Fatal(err)
		}
		socket := filepath.Join(t.TempDir(), "agent.sock")
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		served := make(chan struct{})
		t.Cleanup(func() { listener.Close(); <-served })
		go func() {
			defer close(served)
			connection, err := listener.Accept()
			if err == nil {
				defer connection.Close()
				agent.ServeAgent(ring, connection)
			}
		}()
		t.Setenv("SSH_AUTH_SOCK", socket)
		agentProfile := p
		agentProfile.SSHKeyFile = ""
		agentProfile.SSHAgent = true
		if err := SaveRepositoryCredentialProfile(root, "repo", agentProfile); err != nil {
			t.Fatal(err)
		}
		assertBootstrapRead(t, "ssh://git@"+ln.Addr().String()+"/repo.git", "ssh", root)
		listener.Close()
		<-served
		if err := SaveRepositoryCredentialProfile(root, "repo", p); err != nil {
			t.Fatal(err)
		}
	}
	wrong, _ := sshFixtureKey(t)
	os.WriteFile(hosts, []byte(knownhosts.Line([]string{ln.Addr().String()}, wrong.PublicKey())+"\n"), 0600)
	if _, err := repo.Fetch(context.Background()); err == nil {
		t.Fatal("wrong host key accepted")
	}
	os.WriteFile(hosts, []byte(knownhosts.Line([]string{ln.Addr().String()}, host.PublicKey())+"\n"), 0600)
	_, wrongPrivate := sshFixtureKey(t)
	os.WriteFile(keyFile, wrongPrivate, 0600)
	if _, err := repo.Fetch(context.Background()); err == nil {
		t.Fatal("wrong identity accepted")
	}
	os.WriteFile(hosts, []byte(knownhosts.Line([]string{"unknown.invalid:22"}, host.PublicKey())+"\n"), 0600)
	if _, err := repo.Fetch(context.Background()); !errors.Is(err, ErrRepositoryCredentials) {
		t.Fatal("unregistered host did not fail closed before dialing", err)
	}

}
func serveSSHGit(stream io.ReadWriter, bare, service string) error {
	ep, _ := transport.NewEndpoint(bare)
	srv := server.NewServer(server.DefaultLoader)
	switch service {
	case "git-upload-pack":
		session, err := srv.NewUploadPackSession(ep, nil)
		if err != nil {
			return err
		}
		defer session.Close()
		refs, err := session.AdvertisedReferences()
		if err != nil {
			return err
		}
		if err := refs.Encode(stream); err != nil {
			return err
		}
		req := packp.NewUploadPackRequest()
		if err := req.Decode(struct{ io.Reader }{stream}); err != nil {
			return err
		}
		resp, err := session.UploadPack(context.Background(), req)
		if err != nil {
			return err
		}
		defer resp.Close()
		return resp.Encode(stream)
	case "git-receive-pack":
		session, err := srv.NewReceivePackSession(ep, nil)
		if err != nil {
			return err
		}
		defer session.Close()
		refs, err := session.AdvertisedReferences()
		if err != nil {
			return err
		}
		if err := refs.Encode(stream); err != nil {
			return err
		}
		req := packp.NewReferenceUpdateRequest()
		if err := req.Decode(struct{ io.Reader }{stream}); err != nil {
			return err
		}
		resp, err := session.ReceivePack(context.Background(), req)
		if err != nil {
			return err
		}
		return resp.Encode(stream)
	}
	return ErrGitRepositoryInvalid
}
