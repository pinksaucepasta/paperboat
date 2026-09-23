//go:build darwin || linux

package updated

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"testing"
)

func TestAutomaticUpdatesDisabledDoesNotResolveOrStage(t *testing.T) {
	// There is deliberately no TUF source, manager, or filesystem state. An
	// automatic request must stop at the durable policy before touching any.
	s := &Service{config: Config{Active: workerupdate.Release{Version: "source-build"}, AutomaticUpdates: false}}
	result, err := s.queueActivation(context.Background(), false)
	if err != nil || result.Version != "source-build" || result.Updated {
		t.Fatalf("disabled automatic request: %+v %v", result, err)
	}
}
