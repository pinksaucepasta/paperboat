package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/doctor"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/spf13/cobra"
)

// pbmac 2026-11-05 (support_d1317b73): pb status said Path none and every connection to the peer failed, while
// pb doctor said healthy. With no machine selected, doctor names the path to each machine in use and fails on
// a machine this computer could not reach.
func TestDoctorReportsNoPathToAPeerAsUnhealthy(t *testing.T) {
	noPath := localapi.HealthItem{Code: localapi.NoPathCode, Severity: "error", Title: "No path to macl: the last connection attempt failed", Recovery: "Check that macl is online, then run pb doctor macl.", ETag: localapi.NoPathCode}
	snapshot := localapi.Snapshot{DaemonState: "ready", Machines: []localapi.MachineStatus{
		{ID: "machine_macl", Alias: "macl", SelectedPath: "none", Health: []localapi.HealthItem{noPath}},
		{ID: "machine_hp", Alias: "hp", SelectedPath: "direct"},
		{ID: "machine_idle", Alias: "idle", SelectedPath: "none"},
	}}
	var check doctor.Check
	for _, probe := range doctorProbes(&cobra.Command{}, snapshot, nil) {
		if probe.Code == "peer_paths" {
			check = probe.Run(context.Background())
		}
	}
	if check.Status != doctor.StatusFail || !strings.Contains(check.Summary, "No path to macl") || check.Recovery != noPath.Recovery {
		t.Fatalf("no-path peer check = %+v", check)
	}
	report := doctor.Report{Schema: doctor.SchemaV1, CorrelationID: "pb-doctor-0123456789abcdef", CheckedAt: time.Now(), Overall: "unhealthy", Checks: []doctor.Check{check}}
	if err := report.Validate(); err != nil {
		t.Fatalf("a peer with no path is not unhealthy: %v", err)
	}
	var output bytes.Buffer
	writeDoctorReport(&output, report)
	if !strings.Contains(output.String(), "Paperboat doctor: unhealthy") || !strings.Contains(output.String(), "No path to macl") {
		t.Fatalf("doctor output: %s", output.String())
	}
	// With a path, it is named and healthy.
	snapshot.Machines[0] = localapi.MachineStatus{ID: "machine_macl", Alias: "macl", SelectedPath: "relay", RelayRegion: "india"}
	if check = doctorPeerPathsCheck(snapshot.Machines, time.Now()); check.Status != doctor.StatusPass || check.Summary != "Paths: macl relay, hp direct." {
		t.Fatalf("peer paths check = %+v", check)
	}
	// Selecting the machine fails its selected_path check the same way.
	machine := localapi.MachineStatus{ID: "machine_macl", Alias: "macl", SelectedPath: "none", Health: []localapi.HealthItem{noPath}}
	for _, probe := range doctorMachineProbes(machine) {
		if probe.Code == "selected_path" {
			if got := probe.Run(context.Background()); got.Status != doctor.StatusFail || !strings.Contains(got.Summary, "No path to macl") {
				t.Fatalf("selected_path = %+v", got)
			}
		}
	}
}

// A machine whose open connections use different paths has selected_path "mixed": doctor names them instead of
// saying no path is observed.
func TestDoctorSelectedPathNamesMixedPaths(t *testing.T) {
	machine := localapi.MachineStatus{ID: "machine_team", Alias: "team", SelectedPath: "mixed", ActiveConsumers: 3, TransportConsumers: []localapi.TransportConsumer{{Path: "direct", ActiveConsumers: 2}, {Path: "relay", ActiveConsumers: 1, RelayRegion: "india"}}}
	for _, probe := range doctorMachineProbes(machine) {
		if probe.Code != "selected_path" {
			continue
		}
		got := probe.Run(context.Background())
		if got.Status != doctor.StatusWarning || !strings.Contains(got.Summary, "direct=2, relay/india=1") {
			t.Fatalf("selected_path for mixed paths = %+v", got)
		}
		return
	}
	t.Fatal("no selected_path probe")
}

// L149 (fleet outage 11-12): pb doctor said healthy on every machine while every peer was unreachable, because a path
// was "selected". From the passive record only: online machines tried inside the window and none reached is a warning
// naming one machine; a recent success, no attempt at all, or an old failure is not.
func TestDoctorWarnsWhenNoOnlinePeerWasReachedRecently(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	at := func(ago time.Duration) *time.Time { value := now.Add(-ago); return &value }
	machine := func(alias string, reached, failed *time.Time, path string) localapi.MachineStatus {
		return localapi.MachineStatus{ID: "id_" + alias, Alias: alias, Eligible: true, RuntimeState: "ready", SelectedPath: path, LastReachedAt: reached, LastFailedAt: failed}
	}
	for name, test := range map[string]struct {
		machines []localapi.MachineStatus
		want     string
	}{
		"tried, none reached, a relay path selected": {[]localapi.MachineStatus{machine("w", nil, at(time.Minute), "relay"), machine("m", at(time.Hour), at(5*time.Minute), "none")}, doctor.StatusWarning},
		"one reached recently":                       {[]localapi.MachineStatus{machine("w", nil, at(time.Minute), "relay"), machine("m", at(time.Minute), nil, "none")}, doctor.StatusPass},
		"no attempt at all":                          {[]localapi.MachineStatus{machine("w", nil, nil, "none"), machine("m", nil, nil, "none")}, doctor.StatusPass},
		"failure older than the window":              {[]localapi.MachineStatus{machine("w", nil, at(time.Hour), "none")}, doctor.StatusPass},
		"offline machine does not count":             {[]localapi.MachineStatus{{ID: "id_o", Alias: "o", Eligible: true, RuntimeState: "offline", SelectedPath: "none", LastFailedAt: at(time.Minute)}}, doctor.StatusPass},
	} {
		t.Run(name, func(t *testing.T) {
			check := doctorPeerPathsCheck(test.machines, now)
			if check.Status != test.want {
				t.Fatalf("status = %v (%s), want %v", check.Status, check.Summary, test.want)
			}
			if test.want == doctor.StatusWarning && (!strings.Contains(check.Summary, "last 10 minutes") || !strings.Contains(check.Summary, "to w ") || !strings.Contains(check.Recovery, "pb doctor w")) {
				t.Fatalf("warning = %q / %q, want the window and one example machine", check.Summary, check.Recovery)
			}
		})
	}
}
