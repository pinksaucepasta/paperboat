package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type toolBackend struct {
	exec func(context.Context, ExecArgs) (ExecResult, error)
	send func(context.Context, SendArgs) (SendResult, error)
}

func (*toolBackend) Machines(_ context.Context, limit int) (MachinePage, error) {
	return MachinePage{Machines: []Machine{{ID: "machine_1", Name: "build", Online: true}}, Total: limit + 1, Truncated: true}, nil
}
func (b *toolBackend) Exec(ctx context.Context, args ExecArgs) (ExecResult, error) {
	return b.exec(ctx, args)
}
func (b *toolBackend) SendFile(ctx context.Context, args SendArgs) (SendResult, error) {
	return b.send(ctx, args)
}

func connectTools(t *testing.T, backend Backend) *mcp.ClientSession {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	finished := make(chan error, 1)
	go func() { finished <- New("test", backend).Serve(ctx, serverConn, serverConn) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "test"}, nil)
	session, err := client.Connect(ctx, &mcp.IOTransport{Reader: clientConn, Writer: clientConn}, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = session.Close()
		cancel()
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Error("MCP server did not stop on disconnect")
		}
	})
	return session
}

func TestStdioToolsPreserveArgumentsAndUnknownOutcome(t *testing.T) {
	var sent SendArgs
	b := &toolBackend{
		exec: func(_ context.Context, args ExecArgs) (ExecResult, error) {
			if args.Machine != "build" || len(args.Argv) != 2 || args.Argv[1] != "literal; no shell" || args.Stdin != "input" {
				t.Errorf("tool changed exact command arguments: %+v", args)
			}
			return ExecResult{OperationID: "op_1", Outcome: "unknown", ErrorCode: "exec_start_uncertain"}, nil
		},
		send: func(_ context.Context, args SendArgs) (SendResult, error) {
			sent = args
			return SendResult{Receipt: map[string]any{"batch_id": "batch_1"}}, nil
		},
	}
	session := connectTools(t, b)
	list, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_machines", Arguments: map[string]any{}})
	if err != nil || list.IsError {
		t.Fatalf("inventory: %v, %+v", err, list)
	}
	encoded, _ := json.Marshal(list.StructuredContent)
	var page MachinePage
	if err := json.Unmarshal(encoded, &page); err != nil || page.Total != 101 || !page.Truncated || page.Machines[0].ID != "machine_1" {
		t.Fatalf("bounded inventory lost its total: %s", encoded)
	}
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "exec", Arguments: ExecArgs{Machine: "build", Argv: []string{"echo", "literal; no shell"}, Stdin: "input"}})
	if err != nil || !result.IsError {
		t.Fatalf("uncertain execution must be a structured tool failure: %v, %+v", err, result)
	}
	encoded, _ = json.Marshal(result.StructuredContent)
	var execution ExecResult
	if json.Unmarshal(encoded, &execution) != nil || execution.Outcome != "unknown" || execution.OperationID != "op_1" {
		t.Fatalf("uncertain outcome lost: %s", encoded)
	}
	result, err = session.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_file", Arguments: SendArgs{Machine: "build", Path: "/artifacts/build.zip"}})
	if err != nil || result.IsError || sent.Path != "/artifacts/build.zip" || sent.Machine != "build" {
		t.Fatalf("send dispatch: %v, %+v, %+v", err, result, sent)
	}
}

func TestStdioCancellationAndDisconnectStopActiveTool(t *testing.T) {
	started, stopped := make(chan struct{}, 2), make(chan struct{}, 2)
	b := &toolBackend{exec: func(ctx context.Context, _ ExecArgs) (ExecResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		stopped <- struct{}{}
		return ExecResult{}, ctx.Err()
	}}
	session := connectTools(t, b)
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan error, 1)
	go func() {
		_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "exec", Arguments: ExecArgs{Machine: "build", Argv: []string{"wait"}}})
		finished <- err
	}()
	awaitTool(t, started)
	busy, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_machines", Arguments: map[string]any{}})
	if err != nil || !busy.IsError {
		t.Fatalf("concurrent tool was not refused: %v, %+v", err, busy)
	}
	cancel()
	awaitTool(t, stopped)
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("tool cancellation lost: %v", err)
	}
	go func() {
		_, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "exec", Arguments: ExecArgs{Machine: "build", Argv: []string{"wait"}}})
		finished <- err
	}()
	awaitTool(t, started)
	_ = session.Close()
	awaitTool(t, stopped)
	<-finished
}

func awaitTool(t *testing.T, event <-chan struct{}) {
	t.Helper()
	select {
	case <-event:
	case <-time.After(time.Second):
		t.Fatal("tool lifecycle did not advance")
	}
}

func TestInvalidToolArgumentsNeverReachBackend(t *testing.T) {
	var calls atomic.Int32
	session := connectTools(t, &toolBackend{exec: func(context.Context, ExecArgs) (ExecResult, error) {
		calls.Add(1)
		return ExecResult{}, nil
	}})
	for _, args := range []ExecArgs{
		{Machine: "build"},
		{Machine: "build", Argv: []string{"echo", "bad\x00argument"}},
		{Machine: "build", Argv: []string{"wait"}, TimeoutSeconds: 301},
		{Machine: "build", Argv: []string{"cat"}, Stdin: strings.Repeat("x", MaxOutputBytes+1)},
	} {
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "exec", Arguments: args})
		if err != nil || !result.IsError {
			t.Fatalf("invalid input was accepted: %v, %+v", err, result)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid arguments reached the command owner")
	}
}

type discardCloser struct{ io.Writer }

func (discardCloser) Close() error { return nil }

func TestOversizedStdioFrameClosesSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	input := io.NopCloser(strings.NewReader(strings.Repeat("x", MaxMessageBytes+1) + "\n"))
	if err := New("test", &toolBackend{}).Serve(ctx, input, discardCloser{io.Discard}); err == nil || ctx.Err() != nil {
		t.Fatalf("oversized frame was not rejected at the transport boundary: %v", err)
	}
}
