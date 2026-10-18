package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	servepkg "github.com/pinksaucepasta/paperboat/internal/serve"
)

// A device run showed `pb preview --ttl 30s` ending with the generic
// "could not finish the command" text because the lease expired before
// publication finished, and a closed origin port did the same.
func TestPreviewRefusalsStateWhatToDo(t *testing.T) {
	expired := fmt.Errorf("create preview: %w", errors.Join(preview.ErrLeaseExpired))
	for _, test := range []struct {
		name     string
		err      error
		contains string
		sentinel error
	}{
		{"origin", commandRejection{reason: commandRejectPreviewOrigin}, "Start your app", ErrPreviewOriginUnavailable},
		{"expired", commandRejection{reason: commandRejectPreviewExpired}, "longer --ttl", preview.ErrLeaseExpired},
		{"timeout", commandRejection{reason: commandRejectPreviewTimeout}, "did not become ready", servepkg.ErrPreviewReadinessTimeout},
		{"domain", commandRejection{reason: commandRejectPreviewDomainTimeout}, "DNS records", servepkg.ErrPreviewReadinessTimeout},
	} {
		failure := classifyCommandFailure(test.err)
		if failure.kind != commandRejected {
			t.Fatalf("%s: kind = %v, want rejected", test.name, failure.kind)
		}
		if message := userFacingError(test.err); !strings.Contains(message, test.contains) {
			t.Fatalf("%s: message %q lacks %q", test.name, message, test.contains)
		}
		if !errors.Is(test.err, test.sentinel) {
			t.Fatalf("%s: refusal does not match its sentinel", test.name)
		}
		var output bytes.Buffer
		_ = writeCLIJSONError(&output, test.err)
		if !strings.Contains(output.String(), `"operation_refused"`) || !strings.Contains(output.String(), test.contains) {
			t.Fatalf("%s: JSON = %q", test.name, output.String())
		}
	}
	if !onlyCause(expired, preview.ErrLeaseExpired) {
		t.Fatal("a lone lease-expired cause must become the refusal")
	}
	if onlyCause(errors.Join(expired, errors.New("lease release failed")), preview.ErrLeaseExpired) {
		t.Fatal("a cleanup failure joined beside the cause must not be hidden")
	}
}
