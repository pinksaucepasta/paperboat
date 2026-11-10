//go:build windows

package hostruntimecmd

import (
	"errors"
	"fmt"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
	"github.com/pinksaucepasta/paperboat/internal/windowsopenssh"
)

func TestOpenSSHUnobtainableCrossesTheElevationBoundaryAsACode(t *testing.T) {
	openssh := elevation.Request{Operation: elevation.OperationOpenSSH}
	unobtainable := fmt.Errorf("%w: no App Installer", windowsopenssh.ErrInstallerUnavailable)
	var remote *elevation.RemoteError
	if err := openSSHUnobtainableAsRemote(openssh, unobtainable); !errors.As(err, &remote) || remote.Code != elevation.CodeOpenSSHUnobtainable {
		t.Fatalf("an unobtainable package was not mapped: %v", err)
	}
	refusal := fmt.Errorf("%w: unsigned", windowsopenssh.ErrUntrustedBinary)
	if err := openSSHUnobtainableAsRemote(openssh, errors.Join(unobtainable, refusal)); !errors.Is(err, windowsopenssh.ErrUntrustedBinary) {
		t.Fatalf("a trust refusal was hidden behind the code: %v", err)
	}
	if err := openSSHUnobtainableAsRemote(elevation.Request{Operation: elevation.OperationRuntimeService}, unobtainable); !errors.Is(err, unobtainable) {
		t.Fatalf("another operation's error was mapped: %v", err)
	}
	if err := openSSHUnobtainableAsRemote(openssh, nil); err != nil {
		t.Fatalf("success was turned into an error: %v", err)
	}
}
