package main

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestHomeFileIndexConfigurationFailureIsObserved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", "")
	} else {
		t.Setenv("HOME", "")
	}
	const reference = "support_01234567-89ab-4def-8123-456789abcdef"
	faults := make(chan errorreport.Fault, 1)
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		faults <- fault
	})
	defer restore()
	primeHomeFileIndex(supportref.WithContext(t.Context(), reference))
	select {
	case fault := <-faults:
		if fault.Component != "paperboat-cli" || fault.Operation != "send" || fault.Stage != "command" || fault.Code != "command_failed" || fault.SupportReference != reference {
			t.Fatalf("file-index setup failure lost its diagnostic ownership: %+v", fault)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("file-index setup failure was silently discarded")
	}
}
