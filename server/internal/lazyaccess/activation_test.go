package lazyaccess

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"
)

func TestRuntimeBootValidation(t *testing.T) {
	for _, boot := range []string{"0123456789ABCdef_-", "boot_lazy_runtime_1234567890"} {
		if !validRuntimeBoot(boot) {
			t.Errorf("valid boot rejected: %q", boot)
		}
	}
	for _, boot := range []string{"short", "boot_lazy_1234567/", "boot_lazy_1234567.", "boot_lazy_1234567é", "boot_lazy_1234567\n", "boot_lazy_1234567\\"} {
		if validRuntimeBoot(boot) {
			t.Errorf("invalid boot accepted: %q", boot)
		}
	}
}

func TestActivationAuthorizationDeadline(t *testing.T) {
	denied := errors.New("denied")
	if err := checkActivation(context.Background(), func(context.Context) error { return denied }); !errors.Is(err, denied) {
		t.Fatal("live authorization denial changed", err)
	}
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := checkActivation(expired, func(ctx context.Context) error { return ctx.Err() }); !activationCode(err, "activation_timeout") {
		t.Fatal("deadline did not produce activation_timeout", err)
	}
}

func TestActivatePreservesAuthorizationCauseWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	secret := "private authorization storage detail"
	authorizationErr := errors.New(secret)
	cancellationCause := errors.New("authorization request superseded")
	cancel(cancellationCause)
	_, err := (&Activator{}).Activate(ctx, ActivationRequest{
		PolicyID:          "policy_1",
		OwnerAccountID:    "account_1",
		PolicyGeneration:  1,
		AuthorityDeadline: time.Now().Add(time.Minute),
		Check:             func(context.Context) error { return authorizationErr },
	})
	var activationErr *ActivationError
	if !errors.As(err, &activationErr) || activationErr.Code != "activation_timeout" || !errors.Is(err, authorizationErr) || !errors.Is(err, cancellationCause) || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled authorization lost its cause: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("activation error exposed private authorization detail: %v", err)
	}
}

func TestActivationSessionIdentity(t *testing.T) {
	value, err := activationSessionID()
	if err != nil {
		t.Fatal(err)
	}
	suffix, ok := strings.CutPrefix(value, "session_")
	id, err := uuid.Parse(suffix)
	if !ok || err != nil || id.Version() != 4 || id.Variant() != uuid.RFC4122 || id.String() != suffix {
		t.Fatalf("invalid generated session ID %q", value)
	}
}
