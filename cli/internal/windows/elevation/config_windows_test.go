//go:build windows

package elevation

import (
	"encoding/json"
	"testing"
	"time"
)

func TestConfigServiceElevationRejectsCallerControlledPayload(t *testing.T) {
	for _, action := range []string{ActionConfigInstall, ActionConfigRemove} {
		now := time.Now()
		req := Request{Schema: SchemaV1, RequestID: "config", OwnerSID: "S-1-5-21-1-2-3-1001", Operation: OperationRuntimeService, Action: action, CancelPath: `C:\cancel`, CreatedAt: now, ExpiresAt: now.Add(RuntimeActivationDuration)}
		if err := validateRequest(req); err != nil {
			t.Fatal(err)
		}
		if operationDuration(req.Operation, action) != RuntimeActivationDuration {
			t.Fatal("config activation deadline not bounded")
		}
		req.Payload = json.RawMessage(`{"executable":"C:\\caller.exe"}`)
		if err := validateRequest(req); err == nil {
			t.Fatal("accepted caller-controlled config service payload")
		}
	}
}
