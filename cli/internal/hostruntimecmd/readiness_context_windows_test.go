//go:build windows

package hostruntimecmd

import (
	"context"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
)

func TestReadinessContextOutlivesAnExhaustedInstallBudget(t *testing.T) {
	operation, cancelOperation := context.WithTimeout(context.Background(), time.Hour)
	defer cancelOperation()
	install, cancelInstall := context.WithTimeout(operation, time.Nanosecond) // the install phases used all of theirs
	defer cancelInstall()
	<-install.Done()
	ready, cancelReady := readinessContext(operation)
	defer cancelReady()
	if ready.Err() != nil {
		t.Fatalf("readiness context is already done: %v", ready.Err())
	}
	deadline, ok := ready.Deadline()
	if !ok || time.Until(deadline) < elevation.ReadinessHostdWindow+elevation.ReadinessDaemonBudget {
		t.Fatalf("readiness deadline %v leaves less than both windows", time.Until(deadline))
	}
}
