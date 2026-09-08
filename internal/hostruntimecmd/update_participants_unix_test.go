//go:build darwin || linux

package hostruntimecmd

import (
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

func TestUpdateProbeOutputBoundIncludesCopy(t *testing.T) {
	var output boundedUpdateProbeOutput
	if n, err := io.Copy(&output, strings.NewReader(strings.Repeat("x", 8<<10))); err != nil || n != 8<<10 {
		t.Fatalf("bounded output: %d %v", n, err)
	}
	if _, err := io.Copy(&output, strings.NewReader("x")); err == nil {
		t.Fatal("accepted oversized output")
	}
	if len(output.Bytes()) != 8<<10 {
		t.Fatal("oversized output retained")
	}
}

func TestUnixParticipantsConstructionBeforeInstallCommit(t *testing.T) {
	layout, err := service.DefaultLayout(runtime.GOOS)
	if err != nil {
		t.Fatal(err)
	}
	// The constructor must not require install metadata: updater readiness is
	// established before the installer commits that metadata on a fresh install.
	if _, err := newUnixUpdateParticipants(layout.Binary, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := newUnixUpdateParticipants("/tmp/pb", 1000, 1000); err == nil {
		t.Fatal("accepted arbitrary executable")
	}
	if _, err := newUnixUpdateParticipants(layout.Binary, -1, 1000); err == nil {
		t.Fatal("accepted invalid owner")
	}
}
