//go:build darwin || linux

package updated

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"testing"
)

func TestMissingApprovalDoesNotResolveOrStage(t *testing.T) {
	// There is deliberately no TUF source, manager, or filesystem state. An
	// automatic request must stop at the durable policy before touching any.
	s := &Service{config: Config{Active: workerupdate.Release{Version: "source-build"}, AutomaticUpdates: false}}
	result, err := s.queueActivation(context.Background(), "")
	if !errors.Is(err, ErrApprovalRequired) || result.Updated {
		t.Fatalf("disabled automatic request: %+v %v", result, err)
	}
}
