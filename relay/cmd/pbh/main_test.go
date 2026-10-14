package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat-relay/internal/reporting"
)

func TestPbhDisabledReporterKeepsSafeFailureAndProcessReference(t *testing.T) {
	t.Setenv("PAPERBOAT_SENTRY_ENABLED", "false")
	reporter, err := reporting.New("paperboat-selfhost")
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()
	original := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Stderr = original; reader.Close(); writer.Close() })
	os.Stderr = writer
	reference := reporting.Reference()
	ctx := reporting.WithSupportReference(context.Background(), reference)
	var public bytes.Buffer
	code := execute(ctx, reporter, []string{"selfhost", "code", "--state-dir", filepath.Join(t.TempDir(), "PRIVATE_INSTALLATION")}, io.Discard, &public)
	writer.Close()
	local, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{public.String(), string(local)} {
		if strings.Contains(output, "PRIVATE_INSTALLATION") || !strings.Contains(output, reference) {
			t.Fatalf("unsafe or uncorrelated output: %s", output)
		}
	}
	if code != 1 || !strings.Contains(string(local), `"component":"paperboat-selfhost"`) || !strings.Contains(string(local), `"cause":"not_found"`) {
		t.Fatal("selfhost diagnostic fields missing")
	}
	public.Reset()
	if code := execute(ctx, reporter, []string{"unknown"}, io.Discard, &public); code != 1 || !strings.Contains(public.String(), "use pbh install") {
		t.Fatal("owned command guidance lost")
	}
}
