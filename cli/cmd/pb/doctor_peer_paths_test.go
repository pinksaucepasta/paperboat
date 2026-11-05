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
	if check = doctorPeerPathsCheck(snapshot.Machines); check.Status != doctor.StatusPass || check.Summary != "Paths: macl relay, hp direct." {
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
