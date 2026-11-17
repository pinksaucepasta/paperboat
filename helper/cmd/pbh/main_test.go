package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/reporting"
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
	if code := execute(ctx, reporter, []string{"unknown"}, io.Discard, &public); code != 1 || !strings.Contains(public.String(), "run pbh help") {
		t.Fatal("owned command guidance lost")
	}
}

func TestUninstallRequiresSeparateDataConfirmationBeforeMutation(t *testing.T) {
	for _, args := range [][]string{{"uninstall", "--remove-data"}, {"uninstall", "--confirm-remove-data"}} {
		err := run(context.Background(), args, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "requires both") {
			t.Fatal("unconfirmed data deletion reached privileged teardown")
		}
	}
}

// Every command, including the service's own `serve`, has help text and answers --help without
// touching the host.
func TestHelpDescribesEveryCommand(t *testing.T) {
	var top bytes.Buffer
	if err := run(context.Background(), []string{"help"}, &top); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"install", "selfhost code", "status", "doctor", "configure-tls", "uninstall", "serve", "check-host", "version"} {
		if !strings.Contains(top.String(), command) {
			t.Fatalf("pbh help does not list %q", command)
		}
	}
	for _, args := range [][]string{{"install", "--help"}, {"selfhost", "code", "--help"}, {"status", "--help"}, {"doctor", "--help"}, {"configure-tls", "--help"}, {"uninstall", "--help"}, {"serve", "--help"}} {
		var out bytes.Buffer
		if err := run(context.Background(), args, &out); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.HasPrefix(out.String(), "pbh "+strings.Join(args[:len(args)-1], " ")+": ") {
			t.Fatalf("%v: help lacks its description: %.80s", args, out.String())
		}
	}
}
