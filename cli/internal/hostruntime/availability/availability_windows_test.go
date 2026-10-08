package availability

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type windowsImmediateAvailabilityResolver struct{}

func (windowsImmediateAvailabilityResolver) Resolve(context.Context) (Resolution, error) {
	return Resolution{Schema: PolicySchemaV1, UserMachineID: "um_windows", Mode: "keep_awake", Version: 7}, nil
}

type windowsImmediateAvailabilityHost struct{}

func (windowsImmediateAvailabilityHost) Apply(_ context.Context, policy Resolution) (Observation, error) {
	return Observation{
		Schema:             PolicySchemaV1,
		Mode:               policy.Mode,
		Version:            policy.Version,
		Status:             "applied",
		ObservedAt:         time.Now().UTC(),
		HostServiceVersion: "test",
		HostServiceScope:   "system",
		UpdateHealth:       "healthy",
	}, nil
}

func TestWindowsServicePublishesInitialObservationBeforeStartReturns(t *testing.T) {
	service, err := NewService(windowsImmediateAvailabilityResolver{}, windowsImmediateAvailabilityHost{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	observation := service.Observation()
	if observation == nil || observation.Mode != "keep_awake" || observation.Version != 7 || observation.Status != "applied" {
		t.Fatalf("initial Windows availability observation=%+v", observation)
	}
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	defer shutdownCancel()
	if err := service.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsHostClientAcceptsOnlyOwnerPipe(t *testing.T) {
	canonical, err := windowsOwnerHostServicePipe()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{canonical, strings.ToUpper(canonical)} {
		client, err := NewHostClient(path, time.Second)
		if err != nil || client.socketPath != canonical {
			t.Fatalf("owner pipe rejected: %v", err)
		}
	}
	for _, path := range []string{`\\.\pipe\PaperboatHostService`, `\\.\pipe\Other`, canonical + "-invalid", `\\.\pipe\PaperboatHostService-u000000000000000000000000`} {
		if _, err := NewHostClient(path, time.Second); !errors.Is(err, ErrInvalid) {
			t.Fatalf("foreign pipe accepted: %q", path)
		}
		if _, err := dialAvailabilityHostService(context.Background(), path, time.Second); !errors.Is(err, ErrInvalid) {
			t.Fatalf("foreign pipe dial accepted: %q", path)
		}
	}
	for _, timeout := range []time.Duration{0, -time.Second} {
		if _, err := NewHostClient(canonical, timeout); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid timeout accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if conn, err := dialAvailabilityHostService(ctx, canonical, time.Second); !errors.Is(err, context.Canceled) {
		if conn != nil {
			conn.Close()
		}
		t.Fatalf("owner pipe cancellation: %v", err)
	}
}
