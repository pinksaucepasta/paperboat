//go:build darwin || linux

package tunnel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/process"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	hostruntime "github.com/pinksaucepasta/paperboat/internal/hostruntime/runtime"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

func TestNativeTerminalReattachRestoresCoherentScreenWithoutReplayingInput(t *testing.T) {
	host, authorizer, sessions, target := terminalReplayHost(t)
	var committed atomic.Int64
	target.SequenceSink = func(sequence int) { committed.Store(int64(sequence)) }
	attach := func() *helperTerminalConn {
		t.Helper()
		wire := newExecCanaryWire()
		done := make(chan struct{})
		go func() { defer close(done); _ = host.ServeAuthenticated(wire, authorizer) }()
		t.Cleanup(func() {
			_ = wire.Close()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("terminal server did not join")
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := helperHandshake(ctx, wire); err != nil {
			t.Fatal(err)
		}
		connection, err := newInitializedHelperTerminalConn(ctx, wire, &target, 16)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = connection.Close() })
		return connection
	}
	first := attach()
	if _, err := first.Write([]byte("stty -echo; PB_OWNED=retained; printf 'PB_FIRST_%s\\n' \"$PB_OWNED\"\n")); err != nil {
		t.Fatal(err)
	}
	terminalReadUntil(t, first, []byte("PB_FIRST_retained"))
	before, err := sessions.Snapshot(target.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	target.AfterSequence = 0
	second := attach()
	terminalReadUntil(t, second, []byte("PB_FIRST_retained"))
	if _, err := second.Write([]byte("printf 'PB_SECOND_%s\\n' \"$PB_OWNED\"\n")); err != nil {
		t.Fatal(err)
	}
	terminalReadUntil(t, second, []byte("PB_SECOND_retained"))
	after, err := sessions.Snapshot(target.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation != before.Generation {
		t.Fatal("reattach restarted the shell")
	}
	if uint64(committed.Load()) > after.LatestSequence {
		t.Fatal("screen byte length advanced the output cursor")
	}
}

func terminalReadUntil(t *testing.T, connection *helperTerminalConn, marker []byte) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		var output []byte
		buffer := make([]byte, 4096)
		for len(output) < 64<<10 {
			n, err := connection.Read(buffer)
			output = append(output, buffer[:n]...)
			if bytes.Contains(output, marker) {
				done <- nil
				return
			}
			if err != nil {
				done <- err
				return
			}
		}
		done <- io.ErrShortBuffer
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("terminal fixture output unavailable: %v", err)
		}
	case <-time.After(3 * time.Second):
		_ = connection.Close()
		<-done
		t.Fatal("admitted terminal did not restore its retained fixture output")
	}
}

func terminalReplayHost(t *testing.T) (*server.Server, server.Authorizer, *session.Manager, resolver.TerminalTarget) {
	t.Helper()
	root := t.TempDir()
	adapter, err := pty.NewShellAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) }, MaxSessions: 2})
	if err != nil {
		t.Fatal(err)
	}
	shell, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	launcher, err := process.NewShellLauncher(shell, []string{"HOME=" + root, "PATH=/usr/bin:/bin", "TERM=xterm-256color"}, sessions)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := server.NewDispatcher(server.DispatcherConfig{Sessions: sessions, SessionLauncher: launcher, Health: health.New("replay", []string{"terminal.v1", "health.v1"}, nil), WorkspaceRoot: root, Random: rand.Reader})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	claims := auth.Claims{Issuer: "https://control.example.test", Audience: "paperboat-machine", Subject: "owner", JTI: "jti_replay", IssuedAt: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(time.Minute).Unix(), Scope: []string{"terminal:operate"}, CredentialClass: "terminal_operation", EnvironmentID: "environment_replay", MachineID: "machine_replay", UserID: "owner", CLIClientSessionID: "cli_replay", SessionID: "ses_replay"}
	factory, err := hostruntime.NewStaticAuthorizer(hostruntime.StaticAuthConfig{Issuer: claims.Issuer, EnvironmentID: claims.EnvironmentID, MachineID: claims.MachineID, Keys: map[string]ed25519.PublicKey{"key_replay": public}, Clock: execCanaryClock{now}})
	if err != nil {
		t.Fatal(err)
	}
	token := signExecCanaryCredential(t, "key_replay", private, claims)
	authorizer, err := factory(token)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := operation.NewJournal(32)
	if err != nil {
		t.Fatal(err)
	}
	host, err := server.New(server.Config{Negotiator: protocol.Negotiator{Available: map[string]bool{"terminal.v1": true, "health.v1": true}}, Journal: journal, Authorizer: authorizer, Handler: dispatcher, MaxConcurrent: 4, HeartbeatInterval: time.Hour, MutationDeadline: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = host.Shutdown(ctx)
		_ = sessions.Shutdown(ctx)
	})
	return host, authorizer, sessions, resolver.TerminalTarget{SessionID: claims.SessionID, CWD: root, Cols: 80, Rows: 24, ReplayHistory: true}
}
