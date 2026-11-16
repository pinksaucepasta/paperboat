package resolver

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// F21-11: with Paperboat unreachable, a NEW terminal session on a machine the daemon resolved opens offline: no
// credential, an environment from the daemon's snapshot, a fresh session id (the host names the session by the
// stream's operation), and a bounded lifetime. Everything that needs the server stays refused.
func TestResolveOpensANewTerminalSessionOfflineWhenPaperboatIsUnreachable(t *testing.T) {
	unreachable := &url.Error{Op: "Post", URL: "https://api.example.test/v1/machines/um_1/connect", Err: errors.New("network is unreachable")}
	resolved := &ResolvedMachine{ID: "um_1", Name: "My App", State: "online", Generation: 3, EnvironmentID: "env_1"}
	create := &TerminalSessionCreate{Name: "work", CWD: "/srv", IdempotencyKey: "key-1"}

	fc := &fakeClient{connectErr: unreachable}
	before := time.Now()
	info, err := newTestResolver(fc).Resolve(context.Background(), ConnectRequest{Machine: "My App", ResolvedMachine: resolved, CreateTerminalSession: create})
	if err != nil {
		t.Fatalf("offline resolve: %v", err)
	}
	terminal := info.Terminal
	if terminal == nil || !terminal.Auth.Offline || terminal.Auth.Token != "" || terminal.Auth.Ticket != "" || terminal.Auth.ResourceID != "" {
		t.Fatalf("terminal target = %+v, want an offline target without a credential", terminal)
	}
	if terminal.EnvironmentID != "env_1" || terminal.CWD != "/srv" || info.MachineID != "um_1" || info.MachineGeneration != 3 {
		t.Fatalf("info = %+v terminal = %+v", info, terminal)
	}
	if !strings.HasPrefix(terminal.SessionID, "terminal_offline_") || len(terminal.SessionID) > 128 || strings.Trim(terminal.SessionID, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") != "" {
		t.Fatalf("session id %q is not a valid host session id", terminal.SessionID)
	}
	if info.TerminalSession == nil || info.TerminalSession.ID != terminal.SessionID || info.TerminalSession.Name != "work" {
		t.Fatalf("session = %+v", info.TerminalSession)
	}
	expires, err := time.Parse(time.RFC3339, terminal.Auth.ExpiresAt)
	if err != nil || expires.Before(before.Add(offlineTerminalLifetime-time.Minute)) || expires.After(time.Now().Add(offlineTerminalLifetime+time.Minute)) {
		t.Fatalf("expires %q (%v), want about %v from now", terminal.Auth.ExpiresAt, err, offlineTerminalLifetime)
	}

	refusals := map[string]ConnectRequest{
		"attaching to an existing session":     {Machine: "My App", ResolvedMachine: resolved, CreateTerminalSession: create, TerminalSessionID: "umts_1"},
		"a machine the daemon did not resolve": {Machine: "My App", CreateTerminalSession: create},
		"no environment in the snapshot":       {Machine: "My App", ResolvedMachine: &ResolvedMachine{ID: "um_1", Name: "My App", State: "online", Generation: 3}, CreateTerminalSession: create},
		"no session to create":                 {Machine: "My App", ResolvedMachine: resolved},
	}
	for name, request := range refusals {
		if info, err := newTestResolver(&fakeClient{connectErr: unreachable}).Resolve(context.Background(), request); err == nil {
			t.Errorf("%s opened offline: %+v", name, info.Terminal)
		}
	}
	// A refusal from the server is final: only an unreachable server falls back to the host's rules.
	denied := &api.APIError{Status: 403, Code: "forbidden"}
	if info, err := newTestResolver(&fakeClient{connectErr: denied}).Resolve(context.Background(), ConnectRequest{Machine: "My App", ResolvedMachine: resolved, CreateTerminalSession: create}); err == nil {
		t.Fatalf("a refusal opened offline: %+v", info.Terminal)
	}
}

// A daemon that already reports Paperboat unreachable spares a new terminal the request timeout (31-62 s in the
// F21-01 W outage drill): the grant is never asked for. A terminal the host cannot admit offline still asks.
func TestResolveOpensANewTerminalOfflineWithoutAskingWhenTheDaemonReportsAnOutage(t *testing.T) {
	asked := errors.New("the grant was requested")
	resolved := &ResolvedMachine{ID: "um_1", Name: "My App", State: "online", Generation: 3, EnvironmentID: "env_1"}
	create := &TerminalSessionCreate{Name: "work", IdempotencyKey: "key-1"}

	info, err := newTestResolver(&fakeClient{connectErr: asked}).Resolve(context.Background(), ConnectRequest{Machine: "My App", ResolvedMachine: resolved, CreateTerminalSession: create, ControlPlaneUnavailable: true})
	if err != nil || info.Terminal == nil || !info.Terminal.Auth.Offline {
		t.Fatalf("resolve = %+v, %v; want an offline terminal without a request", info, err)
	}
	_, err = newTestResolver(&fakeClient{connectErr: asked}).Resolve(context.Background(), ConnectRequest{Machine: "My App", ResolvedMachine: resolved, CreateTerminalSession: create, TerminalSessionID: "umts_1", ControlPlaneUnavailable: true})
	if !errors.Is(err, asked) {
		t.Fatalf("attaching to an existing session = %v, want the grant request", err)
	}
}
