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

func TestBrowserDomainServiceElevationIsAllowlistedAndRequiresPayload(t *testing.T) {
	if !validOperationAction(OperationRuntimeService, ActionBrowserDomain) {
		t.Fatal("browser-domain action is not allowlisted for runtime-service elevation")
	}
	if validOperationAction(OperationOpenSSH, ActionBrowserDomain) {
		t.Fatal("browser-domain action is allowlisted for OpenSSH elevation")
	}
	if !actionNeedsPayload(OperationRuntimeService, ActionBrowserDomain) {
		t.Fatal("browser-domain action does not require a payload")
	}
	if got := operationDuration(OperationRuntimeService, ActionBrowserDomain); got != RuntimeActivationDuration {
		t.Fatalf("browser-domain action duration = %s, want %s", got, RuntimeActivationDuration)
	}

	now := time.Now().UTC()
	request := Request{
		Schema:     SchemaV1,
		RequestID:  "browser-domain",
		OwnerSID:   "S-1-5-21-1-2-3-1001",
		Operation:  OperationRuntimeService,
		Action:     ActionBrowserDomain,
		Payload:    json.RawMessage(`{"owner":"S-1-5-21-1-2-3-1001","domain":"apps.local.pprbt.dev"}`),
		CancelPath: `C:\cancel`,
		CreatedAt:  now,
		ExpiresAt:  now.Add(RuntimeActivationDuration),
	}
	if err := validateRequest(request); err != nil {
		t.Fatalf("valid browser-domain request rejected: %v", err)
	}

	request.Payload = nil
	if err := validateRequest(request); err == nil {
		t.Fatal("accepted browser-domain request without payload")
	}
	request.Payload = json.RawMessage(`{"owner":"S-1-5-21-1-2-3-1001","domain":"apps.local.pprbt.dev"}`)
	request.OwnerSID = ""
	if err := validateRequest(request); err == nil {
		t.Fatal("accepted browser-domain request without requester owner SID")
	}
}
