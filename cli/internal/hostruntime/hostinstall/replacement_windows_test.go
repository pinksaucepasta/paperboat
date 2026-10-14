//go:build windows

package hostinstall

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"strings"
	"testing"
)

func TestWindowsReplacementRejectsMalformedNativeSnapshotBeforeMutation(t *testing.T) {
	layout, err := service.WindowsUserLayout("S-1-5-21-111-222-333-1001")
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []string{"", "null", `{}`, `{"unknown":true}`, `{} {}`, `{"installation_absent":false}`, `{"installation_absent":true,"source":{}}`, `{"installation_absent":false,"config":{},"source":{}}`, strings.Repeat("x", (128<<10)+1)} {
		if err := (windowsInstallationState{layout: layout}).Restore(context.Background(), []byte(snapshot)); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("invalid snapshot error = %v", err)
		}
	}
}
