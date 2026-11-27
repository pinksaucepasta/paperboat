//go:build unix

package hostruntimecmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// L46: "attempting to restore the previous installation" was part of the failure pb printed after the restore had
// already run. The notice is printed before the rollback starts, and the failure claims no attempt.
func TestRestoreNoticeIsPrintedBeforeTheRollback(t *testing.T) {
	var out bytes.Buffer
	var seen string
	_, err := restoreAfterNotReady(&out, func(cause error) (string, error) {
		seen = out.String()
		return "", cause
	}, errors.New("deadline"))
	if !strings.Contains(seen, "restoring the previous installation") {
		t.Fatalf("notice before the rollback = %q", seen)
	}
	if err == nil || strings.Contains(err.Error(), "attempting") || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("failure = %v", err)
	}
	// The Ctrl-C advice names what repairs this machine, not a clean one.
	if strings.Contains(stillRestoringMessage, "clean") || !strings.Contains(stillRestoringMessage, "pb doctor") {
		t.Fatalf("still-restoring advice = %q", stillRestoringMessage)
	}
}
