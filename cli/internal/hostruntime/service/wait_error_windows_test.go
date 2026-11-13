//go:build windows

package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"golang.org/x/sys/windows/svc"
)

// victus's upgrade hung with a carrier in StartPending and the install said only "context deadline exceeded".
func TestWindowsServiceWaitErrorNamesTheStuckService(t *testing.T) {
	err := windowsServiceWaitError("PaperboatCarrier-x-g2", svc.StartPending, svc.Running, context.DeadlineExceeded)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cause lost: %v", err)
	}
	for _, want := range []string{"PaperboatCarrier-x-g2", "start pending", "sc.exe queryex"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q missing from %q", want, err)
		}
	}
}
