//go:build darwin || linux

package runtime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

type task43JoinFixture struct {
	Endpoint               string `json:"endpoint"`
	EnvironmentID          string `json:"environment_id"`
	MachineID              string `json:"machine_id"`
	AccessSessionID        string `json:"access_session_id"`
	TerminalSessionID      string `json:"terminal_session_id"`
	ActorAccount           string `json:"actor_account"`
	ClientID               string `json:"client_id"`
	IdentityToken          string `json:"identity_token"`
	PrivateKey             string `json:"proof_private_key_base64"`
	InstallationGeneration int64  `json:"installation_generation"`
	ProofHelperID          string `json:"proof_helper_id"`
}
type task43JoinCredentials struct {
	fixture task43JoinFixture
	key     ed25519.PrivateKey
}

func (c *task43JoinCredentials) Token(context.Context) (string, error) {
	return c.fixture.IdentityToken, nil
}
func (c *task43JoinCredentials) Proof(_ context.Context, operation, method, path string, body []byte) ([]byte, error) {
	now := time.Now().UTC()
	digest := sha256.Sum256(body)
	payload, err := json.Marshal(struct {
		HelperID      string    `json:"helper_id"`
		EnvironmentID string    `json:"environment_id"`
		OperationID   string    `json:"operation_id"`
		Method        string    `json:"method"`
		Path          string    `json:"path"`
		BodySHA256    string    `json:"body_sha256"`
		IssuedAt      time.Time `json:"issued_at"`
		ExpiresAt     time.Time `json:"expires_at"`
	}{c.fixture.ProofHelperID, c.fixture.EnvironmentID, operation, method, path, base64.RawURLEncoding.EncodeToString(digest[:]), now, now.Add(time.Minute)})
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Algorithm string `json:"alg"`
		Payload   string `json:"payload"`
		Signature string `json:"signature"`
	}{"EdDSA", base64.RawURLEncoding.EncodeToString(payload), base64.RawURLEncoding.EncodeToString(ed25519.Sign(c.key, payload))})
}

// This consumer runs beside the real PostgreSQL-backed producer on the approved
// Linux target. No mocked HTTP sender, authorization handler or audit store is used.
func TestTask43ConnectedTerminalJoinConsumer(t *testing.T) {
	dir := os.Getenv("PAPERBOAT_TASK43_DIR")
	if dir == "" {
		t.Skip("requires Task43 connected producer fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	marker := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("ready\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	wait := func(name string) {
		t.Helper()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				return
			}
			if _, err := os.Stat(filepath.Join(dir, "stop")); err == nil {
				t.Fatal("producer stopped before handshake")
			}
			select {
			case <-ctx.Done():
				t.Fatal("connected join fixture timed out")
			case <-tick.C:
			}
		}
	}
	defer func() {
		if t.Failed() {
			_ = os.WriteFile(filepath.Join(dir, "stop"), []byte("consumer failed\n"), 0600)
		}
	}()
	wait("fixture.json")
	info, err := os.Stat(filepath.Join(dir, "fixture.json"))
	if err != nil || info.Mode().Perm()&0077 != 0 || info.Size() > 65536 {
		t.Fatal("fixture must be private")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture task43JoinFixture
	if json.Unmarshal(raw, &fixture) != nil {
		t.Fatal("invalid fixture")
	}
	clear(raw)
	key, err := base64.RawURLEncoding.DecodeString(fixture.PrivateKey)
	if err != nil {
		key, err = base64.StdEncoding.DecodeString(fixture.PrivateKey)
	}
	if err != nil || len(key) != ed25519.PrivateKeySize || fixture.ProofHelperID == "" || fixture.ActorAccount == "" || fixture.ClientID == "" {
		t.Fatal("invalid fixture identity")
	}
	defer clear(key)
	credentials := &task43JoinCredentials{fixture: fixture, key: ed25519.PrivateKey(key)}
	sender := &runtimeObservationSender{endpoint: fixture.Endpoint, tokens: credentials, proofs: credentials, operationID: func() (string, error) {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return "", err
		}
		return "join_" + hex.EncodeToString(id[:]), nil
	}, environmentID: fixture.EnvironmentID, machineID: fixture.MachineID, reporterVersion: "task43-test", client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: rejectRuntimePolicyRedirect}}
	root := t.TempDir()
	adapter, err := pty.NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) }, MaxSessions: 1, MaxAttachments: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		clean, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := sessions.Shutdown(clean); err != nil {
			t.Error(err)
		}
	}()
	snapshot, err := sessions.Create(ctx, session.CreateRequest{ID: fixture.TerminalSessionID, Name: "joined", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "printf task43-replay-marker; while :; do sleep 0.1; printf task43-alive; done"}, CWD: root, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := server.NewDispatcher(server.DispatcherConfig{Sessions: sessions, Health: health.New("task43", []string{"terminal.v1"}, nil), SessionLauncher: testSessionLauncher{sessions: sessions, path: "/bin/sh"}, WorkspaceRoot: root, Random: rand.Reader, RecordTerminalJoin: sender.RecordTerminalJoin})
	if err != nil {
		t.Fatal(err)
	}
	authorization := server.Authorization{AccountID: fixture.ActorAccount, ClientID: fixture.ClientID, ResourceID: fixture.AccessSessionID, SessionID: fixture.TerminalSessionID, TerminalRole: server.TerminalRoleViewer, TerminalGeneration: snapshot.Generation}
	first, _ := json.Marshal(map[string]any{"action": "attach", "session_id": fixture.TerminalSessionID, "attachment_id": "att_task43_first"})
	outcome := dispatcher.Handle(ctx, authorization, "terminal.v1", first)
	if outcome.ErrorCode != "" {
		t.Fatalf("connected attach failed: %s", outcome.ErrorCode)
	}
	stream, opened, err := dispatcher.OpenStream(ctx, authorization, "terminal.v1", first, outcome, false)
	if err != nil || !opened {
		t.Fatal("attached replay stream unavailable")
	}
	defer stream.Close()
	readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
	defer readCancel()
	var output []byte
	for !bytes.Contains(output, []byte("task43-replay-marker")) {
		frame, err := stream.Next(readCtx)
		if err != nil {
			t.Fatal("attached terminal output unavailable")
		}
		output = append(output, frame.Data...)
		if frame.Release != nil {
			frame.Release()
		}
		if len(output) > 4096 {
			t.Fatal("unexpected terminal output bound")
		}
	}
	clear(output)
	marker("joined")
	wait("fail")
	second, _ := json.Marshal(map[string]any{"action": "attach", "session_id": fixture.TerminalSessionID, "attachment_id": "att_task43_denied"})
	rejected := dispatcher.Handle(ctx, authorization, "terminal.v1", second)
	if rejected.ErrorCode != "unavailable" || len(rejected.Result) > 0 {
		t.Fatal("audit failure delivered attachment/replay")
	}
	if _, opened, err := dispatcher.OpenStream(ctx, authorization, "terminal.v1", second, rejected, false); err != nil || opened {
		t.Fatal("failed audit opened terminal stream")
	}
	current, err := sessions.Snapshot(fixture.TerminalSessionID)
	if err != nil || len(current.Participants) != 1 || current.Participants[0].AttachmentID != "att_task43_first" {
		t.Fatal("failed audit leaked participant or removed unrelated attachment")
	}
	if !sessions.AttachmentOwnedBy(fixture.TerminalSessionID, "att_task43_first", fixture.ActorAccount, fixture.ClientID) {
		t.Fatal(errors.New("original attachment lost ownership"))
	}
	liveCtx, liveCancel := context.WithTimeout(ctx, 5*time.Second)
	defer liveCancel()
	alive, err := stream.Next(liveCtx)
	if err != nil || len(alive.Data) == 0 {
		t.Fatal("audit failure interrupted original attachment")
	}
	if alive.Release != nil {
		alive.Release()
	}
	marker("done")
}
