package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntimecmd"
)

// pb install past its outer bound: the install is still finishing, so pb shows the message as written, never
// "Paperboat could not finish" or "failed".
func TestStillFinishingInstallIsNotShownAsAFailure(t *testing.T) {
	message := "The Paperboat install is still finishing in the background after 15m0s. Run `pb status` to see when it is done; run `pb install` again only if it reports no installation."
	err := hostruntimecmd.StillFinishingFailure(message, errors.New("signal: killed"))
	shown := commandFailureMessage(err, classifyCommandFailure(err))
	if !strings.HasPrefix(shown, message) || strings.Contains(shown, "could not") || strings.Contains(shown, "failed") {
		t.Fatalf("pb shows %q", shown)
	}
}
