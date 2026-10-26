//go:build darwin || linux || windows

package runtime

import (
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
)

// newProductionUpdaterClient talks to this installation's updater. Each installation has its own updater
// socket (or pipe), which hostd passes to the worker as PAPERBOAT_UPDATED_SOCKET; NewClient refuses an
// empty or malformed value instead of guessing a shared path.
func newProductionUpdaterClient(environ func(string) string) (*updated.Client, error) {
	return updated.NewClient(environ("PAPERBOAT_UPDATED_SOCKET"), 2*time.Second)
}
