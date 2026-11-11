package managedssh

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type sshCommandServerResult struct {
	command string
	err     error
}

type sshCommandServerBehavior func(ssh.Channel, string) error

func startSSHCommandServer(t *testing.T, host, client ssh.Signer, behavior sshCommandServerBehavior) (net.Conn, <-chan sshCommandServerResult) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan sshCommandServerResult, 1)
	go func() {
		result := sshCommandServerResult{}
		defer func() {
			_ = listener.Close()
			done <- result
		}()
		remote, acceptErr := listener.Accept()
		if acceptErr != nil {
			result.err = acceptErr
			return
		}
		defer remote.Close()
		serverConfig := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if !bytes.Equal(key.Marshal(), client.PublicKey().Marshal()) {
				return nil, errors.New("wrong client key")
			}
			return nil, nil
		}}
		serverConfig.AddHostKey(host)
		connection, channels, requests, err := ssh.NewServerConn(remote, serverConfig)
		if err != nil {
			result.err = err
			return
		}
		defer connection.Close()
		go ssh.DiscardRequests(requests)
		newChannel, ok := <-channels
		if !ok || newChannel.ChannelType() != "session" {
			result.err = errors.New("missing SSH session channel")
			return
		}
		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			result.err = err
			return
		}
		defer channel.Close()
		for request := range channelRequests {
			if request.Type != "exec" {
				_ = request.Reply(false, nil)
				continue
			}
			var payload struct{ Command string }
			if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
				_ = request.Reply(false, nil)
				result.err = err
				return
			}
			result.command = payload.Command
			if err := request.Reply(true, nil); err != nil {
				result.err = err
				return
			}
			result.err = behavior(channel, payload.Command)
			return
		}
		result.err = errors.New("SSH exec request missing")
	}()
	local, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	return local, done
}

func completeSSHCommand(channel ssh.Channel, stdout, stderr string, status uint32) error {
	if _, err := io.WriteString(channel, stdout); err != nil {
		return err
	}
	if _, err := io.WriteString(channel.Stderr(), stderr); err != nil {
		return err
	}
	_, err := channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
	return err
}

func authorizedSSHKey(signer ssh.Signer) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
}

func TestRunSSHCommandPreservesOutputStderrNoOutputAndExitStatus(t *testing.T) {
	clientSigner, _ := testKey(t)
	hostSigner, _ := testKey(t)
	tests := []struct {
		name       string
		stdout     string
		stderr     string
		status     uint32
		wantStatus int
	}{
		{name: "output", stdout: "VICTUS_PB_SSH_OK"},
		{name: "no-output"},
		{name: "stderr-exit", stderr: "REMOTE_STDERR", status: 17, wantStatus: 17},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stream, serverDone := startSSHCommandServer(t, hostSigner, clientSigner, func(channel ssh.Channel, _ string) error {
				return completeSSHCommand(channel, test.stdout, test.stderr, test.status)
			})
			var stdout, stderr bytes.Buffer
			err := RunSSHCommand(t.Context(), stream, SSHCommandConfig{
				Address: "hn.machines.example.test:22", User: "root", Command: "/usr/bin/printf canary",
				Signer: clientSigner, AuthorizedHostKeys: []string{authorizedSSHKey(hostSigner)},
				Output: &stdout, ErrorOutput: &stderr,
			})
			if test.wantStatus == 0 {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var exitErr *ssh.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitStatus() != test.wantStatus {
					t.Fatalf("exit error=%v", err)
				}
			}
			if stdout.String() != test.stdout || stderr.String() != test.stderr {
				t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			result := waitSSHCommandServer(t, serverDone)
			if result.err != nil || result.command != "/usr/bin/printf canary" {
				t.Fatalf("server command=%q error=%v", result.command, result.err)
			}
		})
	}
}

type trackedSSHCommandInput struct {
	io.Reader
	closed chan struct{}
	once   sync.Once
}

func (input *trackedSSHCommandInput) Close() error {
	input.once.Do(func() { close(input.closed) })
	return nil
}

type blockingSSHCommandInput struct {
	closed chan struct{}
	once   sync.Once
}

func (input *blockingSSHCommandInput) Read([]byte) (int, error) {
	<-input.closed
	return 0, io.ErrClosedPipe
}

func (input *blockingSSHCommandInput) Close() error {
	input.once.Do(func() { close(input.closed) })
	return nil
}

func TestRunSSHCommandStreamsFinitePipedInput(t *testing.T) {
	clientSigner, _ := testKey(t)
	hostSigner, _ := testKey(t)
	payload := bytes.Repeat([]byte("paperboat-stdin\x00"), 4096)
	stream, serverDone := startSSHCommandServer(t, hostSigner, clientSigner, func(channel ssh.Channel, _ string) error {
		body, err := io.ReadAll(channel)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(body)
		return completeSSHCommand(channel, hex.EncodeToString(digest[:]), "", 0)
	})
	input := &trackedSSHCommandInput{Reader: bytes.NewReader(payload), closed: make(chan struct{})}
	var output bytes.Buffer
	err := RunSSHCommand(t.Context(), stream, SSHCommandConfig{
		Address: "hn.machines.example.test:22", User: "root", Command: "/usr/bin/sha256sum",
		Signer: clientSigner, AuthorizedHostKeys: []string{authorizedSSHKey(hostSigner)},
		Input: input, Output: &output, ErrorOutput: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(payload)
	if output.String() != hex.EncodeToString(want[:]) {
		t.Fatalf("output=%q", output.String())
	}
	select {
	case <-input.closed:
	default:
		t.Fatal("owned input was not closed")
	}
	if result := waitSSHCommandServer(t, serverDone); result.err != nil {
		t.Fatal(result.err)
	}
}

func TestRunSSHCommandRejectsHostKeySubstitutionAndWrongSigner(t *testing.T) {
	clientSigner, _ := testKey(t)
	wrongClient, _ := testKey(t)
	hostSigner, _ := testKey(t)
	wrongHost, _ := testKey(t)
	for _, test := range []struct {
		name    string
		signer  ssh.Signer
		hostKey string
	}{
		{name: "host-substitution", signer: clientSigner, hostKey: authorizedSSHKey(wrongHost)},
		{name: "wrong-signer", signer: wrongClient, hostKey: authorizedSSHKey(hostSigner)},
	} {
		t.Run(test.name, func(t *testing.T) {
			stream, serverDone := startSSHCommandServer(t, hostSigner, clientSigner, func(channel ssh.Channel, _ string) error {
				return completeSSHCommand(channel, "", "", 0)
			})
			err := RunSSHCommand(t.Context(), stream, SSHCommandConfig{
				Address: "hn.machines.example.test:22", User: "root", Command: "true", Signer: test.signer,
				AuthorizedHostKeys: []string{test.hostKey}, Output: io.Discard, ErrorOutput: io.Discard,
			})
			if err == nil {
				t.Fatal("substituted SSH authority was accepted")
			}
			_ = waitSSHCommandServer(t, serverDone)
		})
	}
}

func TestRunSSHCommandCancellationBeforeAndDuringHandshake(t *testing.T) {
	clientSigner, _ := testKey(t)
	hostSigner, _ := testKey(t)
	for _, preCanceled := range []bool{true, false} {
		t.Run(map[bool]string{true: "before", false: "during"}[preCanceled], func(t *testing.T) {
			client, server := net.Pipe()
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				_, _ = io.Copy(io.Discard, server)
				_ = server.Close()
			}()
			ctx, cancel := context.WithCancel(context.Background())
			if preCanceled {
				cancel()
			} else {
				time.AfterFunc(20*time.Millisecond, cancel)
			}
			err := RunSSHCommand(ctx, client, SSHCommandConfig{
				Address: "hn.machines.example.test:22", User: "root", Command: "true", Signer: clientSigner,
				AuthorizedHostKeys: []string{authorizedSSHKey(hostSigner)}, Output: io.Discard, ErrorOutput: io.Discard,
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error=%v", err)
			}
			select {
			case <-serverDone:
			case <-time.After(time.Second):
				t.Fatal("handshake server goroutine leaked")
			}
		})
	}
}

func TestRunSSHCommandCancellationDuringSessionAndEarlyEOF(t *testing.T) {
	clientSigner, _ := testKey(t)
	hostSigner, _ := testKey(t)
	started := make(chan struct{})
	stream, serverDone := startSSHCommandServer(t, hostSigner, clientSigner, func(channel ssh.Channel, _ string) error {
		close(started)
		_, err := io.Copy(io.Discard, channel)
		return err
	})
	ctx, cancel := context.WithCancel(context.Background())
	input := &blockingSSHCommandInput{closed: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- RunSSHCommand(ctx, stream, SSHCommandConfig{
			Address: "hn.machines.example.test:22", User: "root", Command: "sleep 60", Signer: clientSigner,
			AuthorizedHostKeys: []string{authorizedSSHKey(hostSigner)}, Input: input,
			Output: io.Discard, ErrorOutput: io.Discard,
		})
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("session cancellation error=%v", err)
	}
	select {
	case <-input.closed:
	default:
		t.Fatal("cancellation did not close blocking input")
	}
	_ = waitSSHCommandServer(t, serverDone)

	client, remote := net.Pipe()
	_ = remote.Close()
	err := RunSSHCommand(t.Context(), client, SSHCommandConfig{
		Address: "hn.machines.example.test:22", User: "root", Command: "true", Signer: clientSigner,
		AuthorizedHostKeys: []string{authorizedSSHKey(hostSigner)}, Output: io.Discard, ErrorOutput: io.Discard,
	})
	if !errors.Is(err, ErrSSHServerClosed) {
		t.Fatalf("early transport EOF error=%v, want ErrSSHServerClosed", err)
	}
}

// victus -> hp 2026-11-10: the host ended the stream before sshd's banner and `pb ssh hp -- cmd` printed only the
// generic failure text ("ssh: handshake failed: EOF" underneath). A refusal line is named the same way as on Unix.
func TestRunSSHCommandNamesAHostRefusalInsteadOfAHandshakeFailure(t *testing.T) {
	clientSigner, _ := testKey(t)
	hostSigner, _ := testKey(t)
	client, remote := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, remote) }()
	go func() {
		_, _ = remote.Write([]byte(refusalPrefix + RefusedNotSetUp + "\r\n"))
		_ = remote.Close()
	}()
	err := RunSSHCommand(t.Context(), client, SSHCommandConfig{
		Address: "hn.machines.example.test:22", User: "root", Command: "true", Signer: clientSigner,
		AuthorizedHostKeys: []string{authorizedSSHKey(hostSigner)}, Output: io.Discard, ErrorOutput: io.Discard,
	})
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Reason != RefusedNotSetUp {
		t.Fatalf("refused stream error=%v, want RefusedError %q", err, RefusedNotSetUp)
	}
}

func waitSSHCommandServer(t *testing.T, done <-chan sshCommandServerResult) sshCommandServerResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("SSH command server goroutine leaked")
		return sshCommandServerResult{}
	}
}

// C066: the target's Ed25519 host key changed and its ECDSA key did not. OpenSSH negotiates Ed25519 first and
// refuses; pb's own client (Windows `pb ssh -- <command>`, pb ssh doctor) must refuse the same way instead of
// connecting on the unchanged ECDSA key.
func TestRunSSHCommandChecksTheHostKeyOpenSSHWould(t *testing.T) {
	clientSigner, _ := testKey(t)
	oldEd25519, _ := testKey(t)
	newEd25519, _ := testKey(t)
	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecdsaSigner, err := ssh.NewSignerFromKey(ecdsaKey)
	if err != nil {
		t.Fatal(err)
	}
	server, client := net.Pipe()
	go func() {
		config := &ssh.ServerConfig{NoClientAuth: true}
		config.AddHostKey(ecdsaSigner)
		config.AddHostKey(newEd25519)
		if connection, _, _, err := ssh.NewServerConn(server, config); err == nil {
			_ = connection.Close()
		}
		_ = server.Close()
	}()
	err = RunSSHCommand(t.Context(), client, SSHCommandConfig{
		Address: "hn.machines.example.test:22", User: "root", Command: "true", Signer: clientSigner,
		AuthorizedHostKeys: []string{authorizedSSHKey(ecdsaSigner), authorizedSSHKey(oldEd25519)},
		Output:             io.Discard, ErrorOutput: io.Discard,
	})
	if !errors.Is(err, ErrKnownHosts) {
		t.Fatalf("changed Ed25519 key: err = %v, want ErrKnownHosts", err)
	}
}

func TestOpenSSHHostKeyAlgorithmsPutTrustedTypesFirstInOpenSSHOrder(t *testing.T) {
	got := openSSHHostKeyAlgorithms([]HostPublicKey{{Algorithm: ssh.KeyAlgoRSA}, {Algorithm: ssh.KeyAlgoECDSA256}})
	want := []string{ssh.KeyAlgoECDSA256, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}
