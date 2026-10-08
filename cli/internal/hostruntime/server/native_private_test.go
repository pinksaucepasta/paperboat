package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
)

func TestRevalidateNativePrivateRejectsCredentialTargetSubstitution(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	binding := nativeprivate.Binding{Schema: nativeprivate.SchemaV1, ResourceKind: "tunnel", ResourceID: "tun_1", ResourceGeneration: 2, RouteID: "route_1", RouteGeneration: 3, TargetGeneration: 4, OwnerEndpointID: "machine_1", Protocol: "tcp", TargetScheme: "tcp", TargetAddress: "127.0.0.1:22", ExpiresAt: now.Add(time.Minute)}
	raw, _ := json.Marshal(binding)
	claims := auth.Claims{CredentialClass: "native_private", MachineID: "machine_1", ResourceKind: "tunnel", ResourceID: "tun_1", RouteID: "route_1", Protocol: "tcp", TargetScheme: "tcp", TargetAddress: "127.0.0.1:22", ExpectedGeneration: 2, RouteGeneration: 3, TargetGeneration: 4, ExpiresAt: now.Add(2 * time.Minute).Unix()}
	if _, err := RevalidateNativePrivate(Authorization{Value: claims}, string(raw), "machine_1", now); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*auth.Claims){
		"resource":   func(v *auth.Claims) { v.ResourceID = "tun_other" },
		"route":      func(v *auth.Claims) { v.RouteID = "route_other" },
		"generation": func(v *auth.Claims) { v.TargetGeneration++ },
		"protocol":   func(v *auth.Claims) { v.Protocol = "http" },
		"target":     func(v *auth.Claims) { v.TargetAddress = "127.0.0.1:5432" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := claims
			mutate(&changed)
			if !errors.Is(func() error {
				_, err := RevalidateNativePrivate(Authorization{Value: changed}, string(raw), "machine_1", now)
				return err
			}(), ErrNativePrivateBinding) {
				t.Fatal("substitution accepted")
			}
		})
	}
}

func TestNativePrivateFailureKeepsTypedCauseAndSafeProjection(t *testing.T) {
	const privateText = "127.0.0.1:private-target"
	cause := errors.New(privateText)
	failure := classifyNativePrivateFailure("target_connect", "native_private_failed", cause)
	if !errors.Is(failure, cause) || failure.Error() != "native private operation failed" {
		t.Fatalf("failure did not preserve cause behind a safe public message: %v", failure)
	}
	var staged interface{ DiagnosticStage() string }
	var coded interface{ DiagnosticCode() string }
	if !errors.As(failure, &staged) || staged.DiagnosticStage() != "target_connect" ||
		!errors.As(failure, &coded) || coded.DiagnosticCode() != "native_private_failed" {
		t.Fatalf("failure classification = %T %v", failure, failure)
	}
	var observed errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { observed = fault })
	defer restore()
	reportNativePrivateFailure(context.Background(), "target_connect", "native_private_failed", cause, false)
	if observed.Stage != "target_connect" || observed.Code != "native_private_failed" ||
		strings.Contains(strings.Join(observed.ErrorChain, ","), privateText) || strings.Contains(observed.Cause, privateText) {
		t.Fatalf("private native failure projection = %#v", observed)
	}
}

func TestNativePrivateAuthorityRejectionDoesNotHideBackendFailure(t *testing.T) {
	if !expectedNativePrivateAuthorizationRejection(&auth.Error{Code: auth.SignatureInvalid}) {
		t.Fatal("invalid credential was not treated as an expected rejection")
	}
	if expectedNativePrivateAuthorizationRejection(&auth.Error{Code: auth.KeyUnknown, Cause: errors.New("key lookup unavailable")}) {
		t.Fatal("credential key lookup outage was treated as an expected rejection")
	}
	if expectedNativePrivateAuthorizationRejection(errors.Join(context.Canceled, errors.New("backend failed"))) {
		t.Fatal("mixed cancellation and backend failure was suppressed")
	}
}

func TestRevalidateNativePrivateMachineActorAndGenerationFences(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	binding := nativeprivate.Binding{Schema: nativeprivate.SchemaV1, ResourceKind: "machine_service", ResourceID: "machine", ResourceGeneration: 1, RouteID: "tcp:5432", RouteGeneration: 2, TargetGeneration: 3, OwnerEndpointID: "machine", Protocol: "tcp", TargetScheme: "tcp", TargetAddress: "127.0.0.1:5432", ExpiresAt: now.Add(time.Minute), InstallationGeneration: 1, BootID: "boot", PolicyGeneration: 2, AnnouncementGeneration: 3, UserID: "user", CLIClientSessionID: "cli", AccessSessionID: "access"}
	raw, _ := json.Marshal(binding)
	claims := auth.Claims{CredentialClass: "native_private", MachineID: "machine", ResourceKind: "machine_service", ResourceID: "machine", RouteID: "tcp:5432", Protocol: "tcp", TargetScheme: "tcp", TargetAddress: "127.0.0.1:5432", ExpectedGeneration: 1, RouteGeneration: 2, TargetGeneration: 3, ExpiresAt: now.Add(time.Minute).Unix(), InstallationGeneration: 1, BootID: "boot", PolicyGeneration: 2, AnnouncementGeneration: 3, UserID: "user", CLIClientSessionID: "cli", AssignmentID: "access"}
	if _, err := RevalidateNativePrivate(Authorization{Value: claims}, string(raw), "machine", now); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*auth.Claims){"user": func(c *auth.Claims) { c.UserID = "foreign" }, "cli": func(c *auth.Claims) { c.CLIClientSessionID = "foreign" }, "access": func(c *auth.Claims) { c.AssignmentID = "foreign" }, "installation": func(c *auth.Claims) { c.InstallationGeneration++ }, "boot": func(c *auth.Claims) { c.BootID = "new-boot" }, "policy": func(c *auth.Claims) { c.PolicyGeneration++ }, "announcement": func(c *auth.Claims) { c.AnnouncementGeneration++ }} {
		t.Run(name, func(t *testing.T) {
			altered := claims
			change(&altered)
			if _, err := RevalidateNativePrivate(Authorization{Value: altered}, string(raw), "machine", now); !errors.Is(err, ErrNativePrivateBinding) {
				t.Fatal("signed machine fence substitution accepted")
			}
		})
	}
}
