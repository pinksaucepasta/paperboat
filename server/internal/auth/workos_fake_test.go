package auth

import (
	"context"
	"errors"
	"strings"
)

type FakeWorkOSVerifier struct{}

func (FakeWorkOSVerifier) VerifyCallback(_ context.Context, input CallbackInput) (WorkOSProfile, error) {
	if strings.TrimSpace(input.Code) == "" {
		return WorkOSProfile{}, errors.New("workos callback code is required")
	}
	parts := strings.Split(input.Code, ":")
	if len(parts) >= 2 {
		return WorkOSProfile{Subject: strings.TrimSpace(parts[0]), Email: strings.TrimSpace(parts[1]), DisplayName: strings.TrimSpace(strings.Join(parts[2:], ":"))}, nil
	}
	return WorkOSProfile{Subject: "workos_" + input.Code, Email: input.Code + "@example.invalid", DisplayName: input.Code}, nil
}
