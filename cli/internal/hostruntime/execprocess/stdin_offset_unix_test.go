//go:build darwin || linux

package execprocess

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A client that lost a connection resends its stdin journal from an earlier offset; the execution must receive each
// byte once, in order, and never a gap (the pbvm4 update check's exec_stdin mismatch).
func TestResentStdinIsReceivedOnceAndNeverOutOfOrder(t *testing.T) {
	root := t.TempDir()
	manager := testManager(t, root, nil)
	execution, _, err := manager.Start(context.Background(), Request{OperationID: "operation_stdin_offset", CWD: root, Argv: []string{"/bin/cat"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := execution.WaitStarted(ctx); err != nil {
		t.Fatal(err)
	}
	if err := execution.WriteAt(ctx, 0, []byte("hello ")); err != nil {
		t.Fatal(err)
	}
	// The cut connection accepted "world" but it never arrived; the replacement resends the whole journal from 0.
	if err := execution.WriteAt(ctx, 0, []byte("hello world")); err != nil {
		t.Fatal(err)
	}
	// A second full resend (a second cut) adds nothing.
	if err := execution.WriteAt(ctx, 0, []byte("hello world")); err != nil {
		t.Fatal(err)
	}
	if err := execution.WriteAt(ctx, 20, []byte("later")); !errors.Is(err, ErrInputGap) {
		t.Fatalf("a chunk after a gap was written: %v", err)
	}
	if err := execution.CloseInputAt(30); !errors.Is(err, ErrInputGap) {
		t.Fatalf("stdin closed before its bytes arrived: %v", err)
	}
	if err := execution.CloseInputAt(11); err != nil {
		t.Fatal(err)
	}
	if err := execution.CloseInputAt(11); err != nil {
		t.Fatalf("closing stdin twice: %v", err)
	}
	var out strings.Builder
	for _, event := range collect(t, execution) {
		if event.Stream == "stdout" {
			out.Write(event.Data)
		}
	}
	if out.String() != "hello world" {
		t.Fatalf("stdout=%q", out.String())
	}
}
