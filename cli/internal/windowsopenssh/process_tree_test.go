package windowsopenssh

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// The budget of a preparation step kills its command: Run returns at the deadline, not when the command ends.
func TestCommandRunnerStopsAtTheContextDeadline(t *testing.T) {
	name, arguments := "sleep", []string{"30"}
	if runtime.GOOS == "windows" {
		name, arguments = "ping", []string{"-n", "30", "127.0.0.1"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := (CommandRunner{}).Run(ctx, name, arguments...); err == nil || time.Since(started) > 10*time.Second {
		t.Fatalf("command outlived its context: err=%v after %v", err, time.Since(started))
	}
}
