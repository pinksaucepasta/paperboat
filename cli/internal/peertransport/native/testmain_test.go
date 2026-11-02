package native_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

func TestMain(m *testing.M) {
	// The process logger outlives every owner. Start it before per-owner leak
	// snapshots, including when a single network test runs on its own.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := diagnosticlog.Flush(ctx)
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "native test diagnostics startup failed")
		os.Exit(1)
	}
	os.Exit(m.Run())
}
