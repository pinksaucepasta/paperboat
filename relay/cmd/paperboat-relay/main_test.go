package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat-relay/internal/reporting"
)

func TestRelayStartupRetainsFilesystemCause(t *testing.T) {
	originalArgs := os.Args
	t.Cleanup(func() { os.Args = originalArgs })
	private := filepath.Join(t.TempDir(), "PRIVATE_CERTIFICATE")
	os.Args = []string{"paperboat-relay", "-listen", "127.0.0.1:10000", "-wss-listen", "127.0.0.1:10001", "-jwks", "PRIVATE_JWKS", "-tls-cert", private, "-tls-key", "PRIVATE_KEY"}
	err := runContext(context.Background())
	var failure serviceFailure
	var pathError *os.PathError
	if !errors.As(err, &failure) || failure.definition != "service_build" || !errors.As(err, &pathError) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("original startup cause missing: %T", err)
	}
	if reporting.SafeFailureCause(failure.error) != "not_found" {
		t.Fatal("unexpected safe cause")
	}
}

func TestRelayLocalLifecycleKeepsReferenceWhenSDKDisabled(t *testing.T) {
	original := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Stderr = original; reader.Close(); writer.Close() })
	os.Stderr = writer
	reference := reporting.Reference()
	ctx := reporting.WithSupportReference(context.Background(), reference)
	serviceEvent(ctx, nil, "ready", 0)
	serviceEvent(ctx, nil, "shutdown", 0)
	writer.Close()
	decoder := json.NewDecoder(reader)
	for _, code := range []string{"ready", "shutdown"} {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event["code"] != code || event["support_reference"] != reference || event["correlation_id"] != reference || event["component"] != "paperboat-relay" {
			t.Fatalf("lifecycle fields = %#v", event)
		}
	}
	var extra any
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		t.Fatal("unexpected extra lifecycle record")
	}
}

func TestRelayReadinessRejectsFailureAndCancellation(t *testing.T) {
	failures := make(chan error, 1)
	failures <- fs.ErrPermission
	if err := waitReady(context.Background(), func() bool { return true }, failures); !errors.Is(err, fs.ErrPermission) {
		t.Fatal("declared ready after service failure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitReady(ctx, func() bool { return false }, failures); !errors.Is(err, context.Canceled) {
		t.Fatal("readiness ignored cancellation")
	}
	if err := waitReady(context.Background(), func() bool { return true }, failures); err != nil {
		t.Fatal(err)
	}
}
