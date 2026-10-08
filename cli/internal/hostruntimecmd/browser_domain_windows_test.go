//go:build windows

package hostruntimecmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
)

func TestBrowserDomainDispatchBindsPayloadOwnerToRequester(t *testing.T) {
	const (
		requesterSID = "S-1-5-21-1-2-3-1001"
		foreignSID   = "S-1-5-21-1-2-3-1002"
	)
	for _, test := range []struct {
		name       string
		requestSID string
		payloadSID string
	}{
		{name: "foreign payload owner", requestSID: requesterSID, payloadSID: foreignSID},
		{name: "missing requester owner", payloadSID: requesterSID},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload, err := json.Marshal(BrowserDomainRequest{Owner: test.payloadSID, Domain: "apps.local.pprbt.dev"})
			if err != nil {
				t.Fatal(err)
			}

			// If the requester binding regresses, cancellation keeps a guard call
			// from applying system changes while this test reports the failure.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err = dispatchElevatedOperation(ctx, elevation.Request{
				Operation: elevation.OperationRuntimeService,
				Action:    elevation.ActionBrowserDomain,
				OwnerSID:  test.requestSID,
				Payload:   payload,
			})
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "owner does not match") {
				t.Fatalf("request/payload owner mismatch error = %v", err)
			}
		})
	}
}
