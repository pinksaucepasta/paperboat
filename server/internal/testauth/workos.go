package testauth

import (
	"context"
	"errors"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
)

// FakeWorkOSVerifier is a test fixture. The server never selects it at runtime.
type FakeWorkOSVerifier struct{}

func (FakeWorkOSVerifier) VerifyCallback(_ context.Context, input auth.CallbackInput) (auth.WorkOSProfile, error) {
	if strings.TrimSpace(input.Code) == "" {
		return auth.WorkOSProfile{}, errors.New("workos callback code is required")
	}
	parts := strings.Split(input.Code, ":")
	if len(parts) >= 2 {
		return auth.WorkOSProfile{
			Subject:     strings.TrimSpace(parts[0]),
			Email:       strings.TrimSpace(parts[1]),
			DisplayName: strings.TrimSpace(strings.Join(parts[2:], ":")),
		}, nil
	}
	return auth.WorkOSProfile{
		Subject:     "workos_" + input.Code,
		Email:       input.Code + "@example.invalid",
		DisplayName: input.Code,
	}, nil
}
