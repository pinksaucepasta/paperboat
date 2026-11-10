//go:build windows

package hostruntimecmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
)

func TestInstallWithoutAnApprovalPromptRefusesWithTheAuthoredMessage(t *testing.T) {
	previous := consentPromptAvailable
	consentPromptAvailable = func() bool { return false }
	t.Cleanup(func() { consentPromptAvailable = previous })
	_, err := installUpdateSource(context.Background(), "", installsource.Source{}, "", false, "", false, "")
	var failure *CommandFailure
	if err == nil || !errors.As(err, &failure) {
		t.Fatalf("want an authored refusal, got %v", err)
	}
	for _, phrase := range []string{"administrator", "`pb install`"} {
		if !strings.Contains(failure.Error(), phrase) {
			t.Fatalf("refusal %q lacks %q", failure.Error(), phrase)
		}
	}
}
