package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/configsync"
)

// A repository too large to read is refused with what to do, not "could not finish" (a test Mac:
// connecting github/gitignore).
func TestConfigRepositoryReadLimitNamesTheLimit(t *testing.T) {
	err := configRepositoryReadFailure(fmt.Errorf("read: %w", configsync.ErrRepositoryReadLimit))
	if !strings.Contains(userFacingError(err), "larger than config sync reads") {
		t.Fatalf("message = %q", userFacingError(err))
	}
}
