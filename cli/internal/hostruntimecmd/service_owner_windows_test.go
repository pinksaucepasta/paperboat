//go:build windows

package hostruntimecmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
)

func TestElevatedInstallPayloadIsBoundToRequestOwner(t *testing.T) {
	payload, err := json.Marshal(hostinstall.Request{OwnerSID: "S-1-5-21-1-2-3-1002"})
	if err != nil {
		t.Fatal(err)
	}
	err = dispatchElevatedOperation(context.Background(), elevation.Request{Operation: elevation.OperationRuntimeService, Action: elevation.ActionInstall, OwnerSID: "S-1-5-21-1-2-3-1001", Payload: payload})
	if err == nil || !strings.Contains(err.Error(), "owner does not match") {
		t.Fatalf("mismatched owner error = %v", err)
	}
}
