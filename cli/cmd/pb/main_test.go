package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/command"
	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"github.com/spf13/cobra"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

type testApp struct {
	Writer, ErrWriter io.Writer
}

func newApp() *testApp { return &testApp{} }

func (app *testApp) Run(args []string) error {
	root := newRootCommand()
	if app.Writer != nil {
		root.SetOut(app.Writer)
	}
	if app.ErrWriter != nil {
		root.SetErr(app.ErrWriter)
	}
	if len(args) > 0 {
		args = args[1:]
	}
	root.SetArgs(args)
	return root.ExecuteContext(context.Background())
}

func TestDeliveredTransferKeyCleanupWarningIsBoundedAndPreservesSuccess(t *testing.T) {
	var stderr bytes.Buffer
	writeDeliveredTransferKeyCleanupWarning(&stderr)
	if got := stderr.String(); got != deliveredTransferKeyCleanupWarning {
		t.Fatalf("warning=%q", got)
	}
	if stderr.Len() > 160 || !strings.Contains(stderr.String(), "File delivery completed") || strings.Contains(strings.ToLower(stderr.String()), "transfer failed") {
		t.Fatalf("warning does not preserve delivered state: %q", stderr.String())
	}
}

func TestOpenSSHArgumentsPlacesRemoteCommandAfterDestination(t *testing.T) {
	destination := managedssh.Destination{User: "root", Host: "machine.local.example.test", Port: 2222}
	got := openSSHArguments(destination, []string{"printf ssh-ok"}, true)
	want := []string{
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "PreferredAuthentications=publickey",
		"-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no",
		"-p", "2222", "root@machine.local.example.test", "printf ssh-ok",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("openSSHArguments() = %q, want %q", got, want)
	}
}

func TestResolveSSHRequestedUser(t *testing.T) {
	for _, test := range []struct{ target, flag, want string }{
		{target: "root", want: "root"},
		{flag: "deploy", want: "deploy"},
		{target: "root", flag: "root", want: "root"},
	} {
		got, err := resolveSSHRequestedUser(test.target, test.flag)
		if err != nil || got != test.want {
			t.Fatalf("resolveSSHRequestedUser(%q, %q) = %q, %v", test.target, test.flag, got, err)
		}
	}
	if _, err := resolveSSHRequestedUser("root", "deploy"); !errors.Is(err, managedssh.ErrSSHUsernameConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	if _, err := resolveSSHRequestedUser("", "bad user"); !errors.Is(err, managedssh.ErrSSHUsernameInvalid) {
		t.Fatalf("invalid username error = %v", err)
	}
}

func TestPingCommandAndOutputContract(t *testing.T) {
	command, _, err := newRootCommand().Find([]string{"ping"})
	if err != nil {
		t.Fatal(err)
	}
	if command.Use != "ping <machine>" || command.Flags().Lookup("count") == nil || command.Flags().Lookup("timeout") == nil || command.Flags().Lookup("json") == nil {
		t.Fatalf("unexpected ping command contract: %q", command.Use)
	}
	for _, path := range []string{"direct", "peer_relay", "regional_relay", "unknown"} {
		if got := pingPath(path); got != path {
			t.Fatalf("pingPath(%q)=%q", path, got)
		}
	}
	report := pingReport{Schema: "paperboat.ping/v1", MachineID: "machine_01", MachineName: "workstation", Sent: 1, Received: 1, Samples: []pingSample{{Sequence: 1, Path: "regional_relay", ConnectionMS: 12.5}}}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"candidate", "endpoint", "address", "fingerprint", "credential", "token"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("ping JSON exposed forbidden field category %q: %s", forbidden, encoded)
		}
	}
}

func TestUpdateCommandContract(t *testing.T) {
	root := newRootCommand()
	command, _, err := root.Find([]string{"update"})
	if err != nil || command == nil {
		t.Fatalf("find update: %v", err)
	}
	if command.Use != "update" || command.Flags().Lookup("json") == nil {
		t.Fatalf("unexpected update command contract: %q", command.Use)
	}
	for _, path := range [][]string{{"update", "check"}, {"update", "status"}} {
		child, _, childErr := root.Find(path)
		if childErr != nil || child == nil || child.Flags().Lookup("json") == nil {
			t.Fatalf("find %v: command=%v error=%v", path, child, childErr)
		}
	}
}

func TestUpdateWithProgressReportsHumanProgressOnStderr(t *testing.T) {
	oldInterval := updateProgressInterval
	updateProgressInterval = time.Millisecond
	defer func() { updateProgressInterval = oldInterval }()
	command, _, err := newRootCommand().Find([]string{"update"})
	if err != nil {
		t.Fatal(err)
	}
	command.SetContext(context.Background())
	var stderr bytes.Buffer
	command.SetErr(&stderr)
	started := make(chan struct{})
	resultCh := make(chan updated.ControlResponse, 1)
	go func() {
		response, updateErr := updateWithProgress(command, command.Context(), func(context.Context) (updated.ControlResponse, error) {
			close(started)
			time.Sleep(5 * time.Millisecond)
			return updated.ControlResponse{Version: "2026.08.27.47"}, nil
		})
		if updateErr != nil {
			t.Errorf("updateWithProgress error = %v", updateErr)
		}
		resultCh <- response
	}()
	<-started
	response := <-resultCh
	if response.Version != "2026.08.27.47" {
		t.Fatalf("response = %+v", response)
	}
	if !strings.Contains(stderr.String(), "Processing the signed Paperboat update") || !strings.Contains(stderr.String(), "still in progress") {
		t.Fatalf("progress output = %q", stderr.String())
	}
}

func TestUpdateWithProgressDoesNotWriteJSONStdout(t *testing.T) {
	oldInterval := updateProgressInterval
	updateProgressInterval = time.Millisecond
	defer func() { updateProgressInterval = oldInterval }()
	command, _, err := newRootCommand().Find([]string{"update"})
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Flags().Set("json", "true"); err != nil {
		t.Fatal(err)
	}
	command.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	_, err = updateWithProgress(command, command.Context(), func(context.Context) (updated.ControlResponse, error) {
		return updated.ControlResponse{Version: "2026.08.27.47"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("progress contaminated machine output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestUpdateWithProgressHonorsCancellation(t *testing.T) {
	command, _, err := newRootCommand().Find([]string{"update"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	command.SetContext(ctx)
	_, err = updateWithProgress(command, ctx, func(ctx context.Context) (updated.ControlResponse, error) {
		<-ctx.Done()
		return updated.ControlResponse{}, ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestUpdateReturnsStagedResultSoCanonicalCallerCanExit(t *testing.T) {
	response := updated.ControlResponse{Version: "2026.08.27.50", Pending: true}
	result := updateCommandResult("2026.08.27.46", response, nil)
	if !result.ActivationPending || result.CLIUpdated || result.RuntimeUpdated {
		t.Fatalf("result = %+v", result)
	}
	command, _, err := newRootCommand().Find([]string{"update"})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	command.SetOut(&output)
	if err := writeUpdateResult(command, result, response.Version); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "staged") || !strings.Contains(output.String(), "Activation is in progress") || !strings.Contains(output.String(), "pb update status") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestUpdateStatusIncludesNewerVerifiedCandidate(t *testing.T) {
	for _, tc := range []struct{ name, observed, candidate, want string }{
		{"stale observation", "2026.10.08.32", "2026.10.08.33", "2026.10.08.33"},
		{"no observation", "", "2026.10.08.33", "2026.10.08.33"},
		{"newer observation", "2026.10.08.34", "2026.10.08.33", "2026.10.08.34"},
		{"invalid candidate", "2026.10.08.33", "invalid", "2026.10.08.33"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := updated.ControlResponse{Candidate: &workerupdate.PreparedCandidate{Version: tc.candidate}}
			response.Observation.Version = tc.observed
			response.Transaction.ActiveVersion = "2026.10.08.32"
			result := updateStatusCommandResult("2026.10.08.32", response, nil)
			if result.LatestVersion != tc.want || !result.UpdateAvailable {
				t.Fatalf("latest=%q available=%v; want %q and available", result.LatestVersion, result.UpdateAvailable, tc.want)
			}
		})
	}
}

func TestUpdateStatusPendingTransactionReportsTargetWithoutOfferingAnotherUpdate(t *testing.T) {
	response := updated.ControlResponse{Pending: true}
	response.Observation.Version = "2026.10.08.35"
	response.Transaction.ActiveVersion = "2026.10.08.35"
	response.Transaction.CandidateVersion = "2026.10.08.36"
	result := updateStatusCommandResult("2026.10.08.36", response, nil)
	if result.LatestVersion != "2026.10.08.36" || !result.UpdateAvailable {
		t.Fatalf("latest=%q available=%v", result.LatestVersion, result.UpdateAvailable)
	}
	command, _, err := newRootCommand().Find([]string{"update", "status"})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	command.SetOut(&output)
	if err := writeUpdateStatusResult(command, result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Activation: pending") || strings.Contains(output.String(), "Run `pb update`") {
		t.Fatalf("pending status = %q", output.String())
	}
}

func TestUpdateStatusPreservesWindowsActivationState(t *testing.T) {
	response := updated.ControlResponse{Version: "2026.08.27.50", Pending: true, ActivationFailure: "activation_failed"}
	result := updateStatusCommandResult("2026.08.27.46", response, nil)
	if !result.ActivationPending || result.ActivationFailure != "activation_failed" {
		t.Fatalf("result = %+v", result)
	}
	command, _, err := newRootCommand().Find([]string{"update", "status"})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	command.SetOut(&output)
	if err := writeUpdateStatusResult(command, result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Activation: pending") {
		t.Fatalf("output = %q", output.String())
	}
	result.ActivationPending = false
	output.Reset()
	if err := writeUpdateStatusResult(command, result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Activation: failed (activation_failed)") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestUpdateStatusUsesAuthenticatedDaemonVersion(t *testing.T) {
	response := updated.ControlResponse{Version: "owner-build", Pending: false}
	snapshot := &localapi.Snapshot{DaemonState: "ready", DaemonVersion: "daemon-build"}
	result := updateStatusCommandResult("cli-build", response, snapshot)
	if result.RuntimeVersion != "daemon-build" || !result.RuntimeAvailable || result.RuntimeState != "ready" {
		t.Fatalf("result=%+v", result)
	}

	result = updateStatusCommandResult("cli-build", response, nil)
	if result.RuntimeVersion != "" || result.RuntimeAvailable || result.RuntimeState != "" {
		t.Fatalf("absent daemon result=%+v", result)
	}

	starting := &localapi.Snapshot{DaemonState: "starting", DaemonVersion: "daemon-build"}
	result = updateStatusCommandResult("cli-build", response, starting)
	if result.RuntimeVersion != "" || result.RuntimeAvailable || result.RuntimeState != "starting" {
		t.Fatalf("starting daemon result=%+v", result)
	}
}

func TestUpdateCommandResultRequiresCompletedMatchingDaemon(t *testing.T) {
	matching := &localapi.Snapshot{DaemonState: "ready", DaemonVersion: "candidate"}
	for _, test := range []struct {
		name     string
		response updated.ControlResponse
		snapshot *localapi.Snapshot
		wantCLI  bool
		wantRun  bool
	}{
		{name: "pending", response: updated.ControlResponse{Version: "candidate", Updated: true, Pending: true}, snapshot: matching},
		{name: "missing daemon", response: updated.ControlResponse{Version: "candidate", Updated: true}, wantCLI: true},
		{name: "wrong daemon version", response: updated.ControlResponse{Version: "candidate", Updated: true}, snapshot: &localapi.Snapshot{DaemonState: "ready", DaemonVersion: "previous"}, wantCLI: true},
		{name: "matching daemon", response: updated.ControlResponse{Version: "candidate", Updated: true}, snapshot: matching, wantCLI: true, wantRun: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := updateCommandResult("previous", test.response, test.snapshot)
			if result.CLIUpdated != test.wantCLI || result.RuntimeUpdated != test.wantRun {
				t.Fatalf("result=%+v, want cli=%t runtime=%t", result, test.wantCLI, test.wantRun)
			}
		})
	}
}

func TestReadLocalDaemonSnapshotIsReadOnly(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("local API transport is platform-specific")
	}
	root := commandRuntimeTestRoot(t)
	if runtime.GOOS != "windows" {
		home, runtimeRoot := filepath.Join(root, "home"), filepath.Join(root, "runtime")
		for _, directory := range []string{home, runtimeRoot} {
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("HOME", home)
		t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
		t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
		t.Setenv("TMPDIR", runtimeRoot)
	}
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	store, err := localapi.NewSnapshotStore(&localapi.Snapshot{Schema: localapi.SnapshotSchemaV1, Generation: 1, ObservedAt: now, DaemonState: "ready", DaemonVersion: "daemon-build"})
	if err != nil {
		t.Fatal(err)
	}
	serverConfig, err := commandLocalAPIServerConfig(paths.SocketPath, store)
	if err != nil {
		t.Fatal(err)
	}
	server, err := localapi.NewServer(serverConfig)
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, cancelServer := context.WithCancel(context.Background())
	defer cancelServer()
	done := make(chan error, 1)
	go func() { done <- server.Run(serverCtx) }()
	waitForCommandSocket(t, paths.SocketPath)

	previousInstaller := installLocalDaemonService
	installCalls := 0
	installLocalDaemonService = func(context.Context, string, string, string) error {
		installCalls++
		return errors.New("unexpected daemon installation")
	}
	t.Cleanup(func() { installLocalDaemonService = previousInstaller })
	snapshot, err := readLocalDaemonSnapshot(context.Background())
	if err != nil || snapshot.DaemonVersion != "daemon-build" || snapshot.DaemonState != "ready" {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	if installCalls != 0 {
		t.Fatalf("read-only probe invoked installer %d times", installCalls)
	}
	cancelServer()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("server err=%v", err)
	}
}

func TestSignedUpdateAvailableNeverOffersDowngrade(t *testing.T) {
	for _, test := range []struct {
		installed string
		latest    string
		want      bool
	}{
		{installed: "2026.08.22.28", latest: "2026.08.22.29", want: true},
		{installed: "2026.08.22.28", latest: "2026.08.22.28", want: false},
		{installed: "2026.08.22.28", latest: "2026.08.22.25", want: false},
		{installed: "2026.08.22.28", latest: "", want: false},
	} {
		got, err := signedUpdateAvailable(test.installed, test.latest)
		if err != nil {
			t.Fatalf("signedUpdateAvailable(%q, %q): %v", test.installed, test.latest, err)
		}
		if got != test.want {
			t.Fatalf("signedUpdateAvailable(%q, %q) = %t, want %t", test.installed, test.latest, got, test.want)
		}
	}
	if _, err := signedUpdateAvailable("2026.08.22.28", "latest"); err == nil {
		t.Fatal("malformed signed version was accepted")
	}
}

func TestUpdateCheckRequiresManagedUpdater(t *testing.T) {
	oldVersion := buildinfo.Version
	oldSocket := updateControlSocketForCommand
	defer func() {
		buildinfo.Version = oldVersion
		updateControlSocketForCommand = oldSocket
	}()
	buildinfo.Version = "2026.08.18.1"
	updateControlSocketForCommand = func() (string, error) { return filepath.Join(t.TempDir(), "missing-updater.sock"), nil }
	command, _, err := newRootCommand().Find([]string{"update", "check"})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	command.SetContext(context.Background())
	command.SetOut(&output)
	command.SetErr(&output)
	if err := actionUpdateCheck(command, nil); err == nil {
		t.Fatalf("expected managed updater error, output=%q", output.String())
	}
}

func TestUpdateRequiresManagedUpdater(t *testing.T) {
	oldVersion := buildinfo.Version
	oldSocket := updateControlSocketForCommand
	defer func() {
		buildinfo.Version = oldVersion
		updateControlSocketForCommand = oldSocket
	}()
	buildinfo.Version = "2026.08.18.1"
	updateControlSocketForCommand = func() (string, error) { return filepath.Join(t.TempDir(), "missing-updater.sock"), nil }
	command, _, err := newRootCommand().Find([]string{"update"})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	command.SetContext(context.Background())
	command.SetOut(&output)
	command.SetErr(&output)
	if err := actionUpdate(command, nil); err == nil {
		t.Fatalf("expected managed updater error, output=%q", output.String())
	}
}

func TestDoctorCommandAndOutputContract(t *testing.T) {
	command, _, err := newRootCommand().Find([]string{"doctor"})
	if err != nil {
		t.Fatal(err)
	}
	if command.Use != "doctor [machine]" || command.Flags().Lookup("json") == nil {
		t.Fatalf("unexpected doctor command contract: %q", command.Use)
	}
	report := doctorpkg.Report{
		Schema: doctorpkg.SchemaV1, CorrelationID: "pb-doctor-0123456789abcdef", CheckedAt: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC), Overall: "degraded",
		Machine: &doctorpkg.Machine{ID: "machine_01", Alias: "Studio"},
		Checks: []doctorpkg.Check{
			{Category: "transport", Code: "selected_path", Status: doctorpkg.StatusWarning, Summary: "The active Paperboat path uses a regional relay.", Recovery: "Direct connectivity is retried automatically."},
			{Category: "network", Code: "udp_ipv4", Status: doctorpkg.StatusPass, Summary: "IPv4 UDP sockets are available."},
		},
	}
	if err := report.Validate(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	writeDoctorReport(&output, report)
	for _, expected := range []string{"Paperboat doctor: degraded", "Machine: Studio (machine_01)", "selected_path: warning", "Recovery:"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("doctor output %q does not contain %q", output.String(), expected)
		}
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"candidate", "endpoint", "fingerprint", "credential", "authorization", "token", "192.0.2."} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Fatalf("doctor JSON exposed forbidden field category %q: %s", forbidden, encoded)
		}
	}
}

func TestDoctorMachineProbesReportReadinessWithoutSensitiveDetails(t *testing.T) {
	machine := localapi.MachineStatus{ID: "machine_1", Alias: "Studio", Eligible: true, RuntimeState: "ready", SelectedPath: "wss", RelayRegion: "bom", SSHReadiness: "degraded", NATMappingIPv4: "endpoint_independent", NATMappingIPv6: "destination_dependent", CaptivePortal: "suspected", PMTU: "standard", RouterProtocol: "pcp", RouterMapping: "verified", MappingLifetime: "30s_to_2m", UpdateHealth: "healthy"}
	probes := doctorMachineProbes(machine)
	checks := make(map[string]doctorpkg.Check, len(probes))
	for _, probe := range probes {
		checks[probe.Code] = probe.Run(context.Background())
	}
	if checks["machine_runtime"].Status != doctorpkg.StatusPass || checks["selected_path"].Status != doctorpkg.StatusWarning || checks["ssh_readiness"].Status != doctorpkg.StatusWarning || checks["nat_mapping_ipv4"].Status != doctorpkg.StatusPass || checks["nat_mapping_ipv6"].Status != doctorpkg.StatusWarning || checks["captive_portal"].Status != doctorpkg.StatusWarning || checks["path_mtu"].Status != doctorpkg.StatusPass || checks["router_protocol"].Status != doctorpkg.StatusPass || checks["router_mapping"].Status != doctorpkg.StatusPass || checks["mapping_lifetime"].Status != doctorpkg.StatusPass || checks["update_health"].Status != doctorpkg.StatusPass {
		t.Fatalf("checks=%#v", checks)
	}
	encoded, _ := json.Marshal(checks)
	if strings.Contains(string(encoded), "bom") {
		t.Fatalf("machine probes leaked relay detail outside the typed report: %s", encoded)
	}
}

func TestDoctorRouterProtocolCheckUsesOnlyBoundedCategories(t *testing.T) {
	for value, want := range map[string]string{
		"pcp": doctorpkg.StatusPass, "nat_pmp": doctorpkg.StatusPass, "upnp": doctorpkg.StatusPass,
		"none": doctorpkg.StatusUnavailable, "unknown": doctorpkg.StatusUnavailable,
	} {
		check := doctorRouterProtocolCheck(value)
		if check.Status != want || check.Code != "router_protocol" {
			t.Fatalf("value=%q check=%#v", value, check)
		}
		encoded, _ := json.Marshal(check)
		for _, forbidden := range []string{"192.0.2.1", "gateway address", "stun:", "external port", "machine"} {
			if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
				t.Fatalf("value=%q leaked %q: %s", value, forbidden, encoded)
			}
		}
	}
}

func TestDoctorRouterMappingCheckUsesOnlyBoundedCategories(t *testing.T) {
	want := map[string]string{
		"verified":    doctorpkg.StatusPass,
		"unreachable": doctorpkg.StatusWarning,
		"untrusted":   doctorpkg.StatusUnavailable,
		"unavailable": doctorpkg.StatusUnavailable,
		"unknown":     doctorpkg.StatusUnavailable,
	}
	for value, status := range want {
		check := doctorRouterMappingCheck(value)
		if check.Status != status || check.Code != "router_mapping" {
			t.Fatalf("value=%q check=%#v", value, check)
		}
		encoded, _ := json.Marshal(check)
		for _, forbidden := range []string{"192.0.2.1", "stun:", "gateway", "pcp", "upnp", "nat-pmp"} {
			if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
				t.Fatalf("value=%q leaked %q: %s", value, forbidden, encoded)
			}
		}
	}
}

func TestDoctorMappingLifetimeCheckUsesConservativeBuckets(t *testing.T) {
	for value, want := range map[string]string{
		"over_10m":  doctorpkg.StatusPass,
		"2m_to_10m": doctorpkg.StatusPass,
		"30s_to_2m": doctorpkg.StatusPass,
		"under_30s": doctorpkg.StatusWarning,
		"unknown":   doctorpkg.StatusUnavailable,
	} {
		check := doctorMappingLifetimeCheck(value)
		if check.Status != want || check.Code != "mapping_lifetime" {
			t.Fatalf("value=%q check=%#v", value, check)
		}
	}
}

func TestDoctorNativeReachabilityUsesObservedPath(t *testing.T) {
	for _, sample := range []struct {
		path   string
		status string
	}{
		{"direct", doctorpkg.StatusPass},
		{"peer_relay", doctorpkg.StatusWarning},
		{"regional_relay", doctorpkg.StatusWarning},
		{"unknown", doctorpkg.StatusPass},
	} {
		check := doctorPeerCheck(tunnel.NativeProbe{Path: sample.path, Connection: 8 * time.Millisecond})
		if check.Status != sample.status || check.SelectedPath != sample.path {
			t.Fatalf("path=%q check=%#v", sample.path, check)
		}
		if err := check.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDoctorNativeReachabilityHonorsFailureAndCancellation(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		parent context.Context
		load   func(context.Context) (tunnel.NativeProbe, error)
		status string
	}{
		{"success", context.Background(), func(context.Context) (tunnel.NativeProbe, error) { return tunnel.NativeProbe{Path: "direct"}, nil }, doctorpkg.StatusPass},
		{"failure", context.Background(), func(context.Context) (tunnel.NativeProbe, error) { return tunnel.NativeProbe{}, errors.New("offline") }, doctorpkg.StatusFail},
		{"canceled", canceledContext(), func(ctx context.Context) (tunnel.NativeProbe, error) { return tunnel.NativeProbe{}, ctx.Err() }, doctorpkg.StatusUnavailable},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			probes := doctorNativeReachabilityProbe(scenario.parent, time.Second, scenario.load)
			if len(probes) != 1 {
				t.Fatalf("probes=%d", len(probes))
			}
			check := probes[0].Run(context.Background())
			if check.Status != scenario.status || check.Code != "peer_reachability" {
				t.Fatalf("check=%#v", check)
			}
		})
	}
}

func TestDoctorNativeProbeHonorsRunnerDeadlineAndRecordsOriginalCause(t *testing.T) {
	restore := errorreport.Install(nil)
	defer restore()
	reference := supportref.New()
	ctx, cancel := context.WithTimeout(supportref.WithContext(t.Context(), reference), 20*time.Millisecond)
	defer cancel()
	var fault errorreport.Fault
	restoreObserver := errorreport.InstallFaultObserver(func(_ context.Context, observed errorreport.Fault) { fault = observed })
	defer restoreObserver()
	probe := doctorNativeReachabilityProbe(context.Background(), time.Second, func(loadCtx context.Context) (tunnel.NativeProbe, error) {
		<-loadCtx.Done()
		return tunnel.NativeProbe{}, loadCtx.Err()
	})[0]
	started := time.Now()
	check := probe.Run(ctx)
	if time.Since(started) > 500*time.Millisecond || check.Status != doctorpkg.StatusUnavailable || !strings.Contains(check.Summary, "timed out") || fault.Cause != "deadline_exceeded" || fault.Stage != "peer_connect" || fault.SupportReference != reference {
		t.Fatalf("probe lost deadline or diagnostic evidence: check=%#v fault=%#v", check, fault)
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestSpecTreeRegistersDeclaredFlagTypes(t *testing.T) {
	called := false
	source := &command.Spec{Name: "fixture", Subcommands: []*command.Spec{{
		Name: "inspect", Action: func(ctx *command.Context) error {
			called = true
			if ctx.String("recovery-key") != "fixture-key" || !ctx.Bool("json") || ctx.String("hours") != "3.5" {
				t.Fatalf("action values: recovery-key=%q json=%t hours=%q", ctx.String("recovery-key"), ctx.Bool("json"), ctx.String("hours"))
			}
			return nil
		}, Flags: []command.Flag{
			&command.StringFlag{Name: "recovery-key", Usage: "key file"},
			&command.BoolFlag{Name: "json", Usage: "JSON output"},
			&command.Float64Flag{Name: "hours", Usage: "hours"},
		},
	}}}
	tree := specTree(source, source.Name)
	entry, _, err := tree.Find([]string{"inspect"})
	if err != nil {
		t.Fatal(err)
	}
	for name, wantType := range map[string]string{"recovery-key": "string", "json": "bool", "hours": "float64"} {
		flag := entry.Flags().Lookup(name)
		if flag == nil || flag.Value.Type() != wantType {
			t.Fatalf("flag %q=%v, want type %q", name, flag, wantType)
		}
	}
	tree.SetOut(io.Discard)
	tree.SetErr(io.Discard)
	tree.SetArgs([]string{"inspect", "--recovery-key", "fixture-key", "--json", "--hours", "3.5"})
	if err := tree.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("action was not called")
	}
}

func TestSpecTreeRegistersEveryDeclaredBoolFlag(t *testing.T) {
	for _, source := range []*command.Spec{authCommand(), configCommand(), inboxCommand()} {
		tree := specTree(source, source.Name)
		for _, childSpec := range source.Subcommands {
			entry, _, err := tree.Find([]string{childSpec.Name})
			if err != nil {
				t.Fatalf("%s %s: %v", source.Name, childSpec.Name, err)
			}
			for _, declared := range childSpec.Flags {
				boolFlag, ok := declared.(*command.BoolFlag)
				if !ok {
					continue
				}
				registered := entry.Flags().Lookup(boolFlag.Name)
				if registered == nil || registered.Value.Type() != "bool" {
					t.Fatalf("%s %s --%s=%v", source.Name, childSpec.Name, boolFlag.Name, registered)
				}
			}
		}
	}
}

func specTreeActionTestCommand(t *testing.T, source *command.Spec, childName string, action command.Action) *cobra.Command {
	t.Helper()
	for _, child := range source.Subcommands {
		if child.Name != childName {
			continue
		}
		childCopy := *child
		childCopy.Action = action
		return specTree(&command.Spec{Name: source.Name, Usage: source.Usage, Subcommands: []*command.Spec{&childCopy}}, source.Name)
	}
	t.Fatalf("%s subcommand %q not found", source.Name, childName)
	return nil
}

func TestSpecTreeExecutesDeclaredFlagsIntoActions(t *testing.T) {
	tests := []struct {
		name        string
		source      func() *command.Spec
		child       string
		args        []string
		wantStrings map[string]string
		wantBools   map[string]bool
	}{
		{name: "auth login json", source: authCommand, child: "login", args: []string{"login", "--json"}, wantBools: map[string]bool{"json": true}},
		{name: "auth status json", source: authCommand, child: "status", args: []string{"status", "--json"}, wantBools: map[string]bool{"json": true}},
		{name: "config show json", source: configCommand, child: "show", args: []string{"show", "--json"}, wantBools: map[string]bool{"json": true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			tree := specTreeActionTestCommand(t, test.source(), test.child, func(ctx *command.Context) error {
				called = true
				for name, want := range test.wantStrings {
					if got := ctx.String(name); got != want {
						t.Fatalf("%s=%q, want %q", name, got, want)
					}
				}
				for name, want := range test.wantBools {
					if got := ctx.Bool(name); got != want {
						t.Fatalf("%s=%t, want %t", name, got, want)
					}
				}
				return nil
			})
			tree.SetOut(io.Discard)
			tree.SetErr(io.Discard)
			tree.SetArgs(test.args)
			if err := tree.ExecuteContext(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("action was not called")
			}
		})
	}
}
