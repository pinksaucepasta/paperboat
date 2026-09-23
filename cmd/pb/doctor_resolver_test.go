package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"testing"

	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

type doctorResolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f doctorResolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}
func activeResolverSnapshot() localapi.Snapshot {
	return localapi.Snapshot{DeviceSuffix: "pprbt", Machines: []localapi.MachineStatus{{ID: "machine_b", Alias: "Backup", Eligible: true, RuntimeState: "offline"}, {ID: "machine_a", Alias: "Studio", Eligible: true, RuntimeState: "ready"}}}
}
func resolvedDoctorDevice(_ context.Context, id string) (doctorResolvedDevice, error) {
	if id != "machine_a" {
		return doctorResolvedDevice{}, errors.New("wrong selection")
	}
	return doctorResolvedDevice{alias: "studio", address: netip.MustParseAddr("127.100.23.45")}, nil
}

func TestDoctorSystemResolutionMatchesAuthoritativeAddress(t *testing.T) {
	check := probeDoctorSystemResolution(t.Context(), activeResolverSnapshot(), nil, resolvedDoctorDevice, doctorResolverFunc(func(_ context.Context, network, host string) ([]netip.Addr, error) {
		if network != "ip" || host != "studio.pprbt" {
			t.Fatalf("lookup %q %q", network, host)
		}
		return []netip.Addr{netip.MustParseAddr("127.100.23.45"), netip.MustParseAddr("127.100.23.45")}, nil
	}))
	if check.Status != doctorpkg.StatusPass || !strings.Contains(check.Recovery, "DNS-over-HTTPS") {
		t.Fatalf("check=%#v", check)
	}
}
func TestDoctorSystemResolutionMismatchAndWrongFamily(t *testing.T) {
	for name, answers := range map[string][]netip.Addr{
		"empty":        {},
		"mismatch":     {netip.MustParseAddr("127.100.23.46")},
		"wrong_family": {netip.MustParseAddr("::1")},
		"mixed":        {netip.MustParseAddr("127.100.23.45"), netip.MustParseAddr("127.100.23.46")},
	} {
		t.Run(name, func(t *testing.T) {
			check := probeDoctorSystemResolution(t.Context(), activeResolverSnapshot(), nil, resolvedDoctorDevice, doctorResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) { return answers, nil }))
			if check.Status != doctorpkg.StatusFail || !strings.Contains(check.Summary, "wrong address") {
				t.Fatalf("check=%#v", check)
			}
			encoded, _ := json.Marshal(check)
			if strings.Contains(string(encoded), "127.100") || strings.Contains(string(encoded), "::1") {
				t.Fatalf("address leaked: %s", encoded)
			}
		})
	}
}
func TestDoctorSystemResolutionCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	check := probeDoctorSystemResolution(ctx, activeResolverSnapshot(), nil, resolvedDoctorDevice, doctorResolverFunc(func(ctx context.Context, _ string, _ string) ([]netip.Addr, error) { return nil, ctx.Err() }))
	if check.Status != doctorpkg.StatusUnavailable || !strings.Contains(check.Summary, "canceled") {
		t.Fatalf("check=%#v", check)
	}
}
func TestDoctorSystemResolutionTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	check := probeDoctorSystemResolution(ctx, activeResolverSnapshot(), nil, resolvedDoctorDevice, doctorResolverFunc(func(ctx context.Context, _ string, _ string) ([]netip.Addr, error) { return nil, ctx.Err() }))
	if check.Status != doctorpkg.StatusUnavailable || !strings.Contains(check.Summary, "deadline") {
		t.Fatalf("check=%#v", check)
	}
}
func TestDoctorSystemResolutionLookupFailure(t *testing.T) {
	check := probeDoctorSystemResolution(t.Context(), activeResolverSnapshot(), nil, resolvedDoctorDevice, doctorResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return nil, errors.New("resolver unavailable")
	}))
	if check.Status != doctorpkg.StatusFail || !strings.Contains(check.Summary, "could not resolve") {
		t.Fatalf("check=%#v", check)
	}
}
func TestDoctorSystemResolutionNoActiveDeviceDoesNotResolve(t *testing.T) {
	snapshot := activeResolverSnapshot()
	snapshot.Machines[1].RuntimeState = "offline"
	called := false
	check := probeDoctorSystemResolution(t.Context(), snapshot, nil, func(context.Context, string) (doctorResolvedDevice, error) {
		called = true
		return doctorResolvedDevice{}, nil
	}, doctorResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		t.Fatal("unexpected OS lookup")
		return nil, nil
	}))
	if called || check.Status != doctorpkg.StatusUnavailable {
		t.Fatalf("called=%v check=%#v", called, check)
	}
}
func TestDoctorSystemResolutionSelectedInactiveDoesNotFallBack(t *testing.T) {
	snapshot := activeResolverSnapshot()
	selected := snapshot.Machines[0]
	called := false
	check := probeDoctorSystemResolution(t.Context(), snapshot, &selected, func(context.Context, string) (doctorResolvedDevice, error) {
		called = true
		return doctorResolvedDevice{}, nil
	}, doctorResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) { return nil, nil }))
	if called || check.Status != doctorpkg.StatusUnavailable {
		t.Fatalf("called=%v check=%#v", called, check)
	}
}
