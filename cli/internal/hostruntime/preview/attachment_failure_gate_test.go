package preview

import (
	"context"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/errorreport"
)

// X-41: the carrier pool re-dials every second while the edge refuses; one repeating failure is one record per interval.
func TestRepeatingDialFailureIsRecordedOncePerInterval(t *testing.T) {
	attachmentFailureLast.Clear()
	var seen int
	defer errorreport.InstallFaultObserver(func(context.Context, errorreport.Fault) { seen++ })()
	for range 10 {
		logAttachmentFailure(context.Background(), "dial_h3", errors.New("tls alert"))
	}
	if seen != 1 {
		t.Fatalf("a repeating dial failure was recorded %d times", seen)
	}
	logAttachmentFailure(context.Background(), "dial_h2", errors.New("tls alert"))
	if seen != 2 {
		t.Fatalf("another transport's failure was recorded %d times in all", seen)
	}
}
