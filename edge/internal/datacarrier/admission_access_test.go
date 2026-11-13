package datacarrier

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPreviewAdmissionAccessModesRequireExactPrivateAuthority(t *testing.T) {
	for _, mode := range []string{"public", "private", "team"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now().UTC()
			admission := testExpectedAdmissionForRegistry(now.Add(time.Hour))
			admission.AccessMode = mode
			if mode != "public" {
				admission.RouteKind = PreviewCarrierPrivateRoute
			}
			registry, err := NewExpectedAdmissionRegistry(ExpectedAdmissionRegistryConfig{NodeID: admission.EdgeNodeID, ProcessEpoch: admission.EdgeProcessEpoch})
			if err != nil {
				t.Fatal(err)
			}
			defer registry.Close()
			if err := registry.Replace([]ExpectedAdmission{admission}, now); err != nil {
				t.Fatalf("install %s preview admission: %v", mode, err)
			}
			open := streamOpenForIdentity(admission.Identity, admission.RouteID, "request_access")
			err = registry.AuthorizeStream(context.Background(), admission.Identity, open)
			if mode == "public" {
				if err != nil {
					t.Fatalf("public stream: %v", err)
				}
			} else if !errors.Is(err, ErrRouteDenied) {
				t.Fatalf("ungranted %s stream accepted: %v", mode, err)
			}
			decision := PrivateAccessDecision{Allowed: true, ExpiresAt: now.Add(time.Minute), ResourceID: admission.PreviewID, RouteID: admission.RouteID, OperationID: admission.OperationID, CarrierSessionID: admission.Identity.SessionID, RouteGeneration: admission.RouteRevision, ProcessGeneration: admission.Identity.ProcessGeneration, ConfigGeneration: admission.Identity.Generation, Protocol: "http"}
			ctx := WithPrivateAccessDecision(context.Background(), decision)
			if err := registry.AuthorizeStream(ctx, admission.Identity, open); err != nil {
				t.Fatalf("granted %s stream: %v", mode, err)
			}
			if mode != "public" {
				foreign := decision
				foreign.ResourceID = "preview_foreign"
				if err := registry.AuthorizeStream(WithPrivateAccessDecision(context.Background(), foreign), admission.Identity, open); !errors.Is(err, ErrRouteDenied) {
					t.Fatalf("foreign preview decision accepted: %v", err)
				}
			}
			if err := registry.Remove(admission); err != nil {
				t.Fatal(err)
			}
			if err := registry.AuthorizeStream(ctx, admission.Identity, open); !errors.Is(err, ErrAdmissionNotExpected) {
				t.Fatalf("revoked admission accepted: %v", err)
			}
		})
	}
}
