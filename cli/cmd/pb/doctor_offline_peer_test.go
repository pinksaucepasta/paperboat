package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/doctor"
	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/spf13/cobra"
)

func commandWithMachineRecord(record func(string) (api.UserMachine, error)) *cobra.Command {
	command := &cobra.Command{}
	command.SetContext(withDoctorDependencies(context.Background(), func(dependencies *doctorDependencies) {
		dependencies.machineRecord = func(_ context.Context, _ *cobra.Command, id string) (api.UserMachine, error) { return record(id) }
	}))
	return command
}

// unreachedPeer is a machine this computer just failed to reach, as the daemon last saw it.
func unreachedPeer(alias, state string) localapi.MachineStatus {
	return localapi.MachineStatus{ID: "machine_" + alias, Alias: alias, Eligible: true, RuntimeState: state, SelectedPath: "none", Health: []localapi.HealthItem{
		{Code: localapi.NoPathCode, Severity: "error", Title: "No path to " + alias + ": the last connection attempt failed", Recovery: "Check that " + alias + " is online, then run pb doctor " + alias + ".", ETag: localapi.NoPathCode},
	}}
}

// Smoke 11-26 LP2: a healthy computer was "unhealthy" because of a sleeping test Mac's record. A machine
// Paperboat lists offline is information; one it lists online and this computer cannot reach is a failure.
func TestDoctorDoesNotFailOnAPeerListedOffline(t *testing.T) {
	relay := localapi.MachineStatus{ID: "machine_hp", Alias: "laptop", Eligible: true, RuntimeState: "ready", SelectedPath: "relay"}
	for name, test := range map[string]struct {
		machines     []localapi.MachineStatus
		status       string
		want, absent string
	}{
		"only an offline peer":      {[]localapi.MachineStatus{unreachedPeer("sleeper", "offline")}, doctor.StatusPass, "No connection to another machine is in use. sleeper is offline.", "No path"},
		"beside a path in use":      {[]localapi.MachineStatus{unreachedPeer("sleeper", "offline"), relay}, doctor.StatusPass, "Paths: laptop relay. sleeper is offline.", "No path"},
		"two offline, one unpaired": {[]localapi.MachineStatus{unreachedPeer("sleeper", "offline"), unreachedPeer("fresh", "stopped")}, doctor.StatusPass, "sleeper, fresh are offline.", "No path"},
		"a removed peer":            {[]localapi.MachineStatus{unreachedPeer("gone", "failed")}, doctor.StatusPass, "No connection to another machine is in use.", "gone"},
		"online beside offline":     {[]localapi.MachineStatus{unreachedPeer("awake", "ready"), unreachedPeer("sleeper", "offline")}, doctor.StatusFail, "No path to awake: the last connection attempt failed. sleeper is offline.", "No path to awake, sleeper"},
	} {
		t.Run(name, func(t *testing.T) {
			check := doctorPeerPathsCheck(test.machines, time.Now())
			if check.Status != test.status || !strings.Contains(check.Summary, test.want) || strings.Contains(check.Summary, test.absent) {
				t.Fatalf("peer paths check = %+v, want %s with %q and without %q", check, test.status, test.want, test.absent)
			}
			if check.Status == doctor.StatusPass && check.Recovery != "" {
				t.Fatalf("a passing check asked for action: %q", check.Recovery)
			}
			if check.Status == doctor.StatusFail && !strings.Contains(check.Recovery, "awake") {
				t.Fatalf("recovery = %q, want the unreachable online machine's", check.Recovery)
			}
		})
	}
}

// The daemon's list lags the server by up to its 5-minute re-list, so a machine stopped a moment ago is still
// cached online: doctor goes by what the server lists now, and by the daemon's view when it does not answer.
func TestDoctorAsksTheServerWhetherAnUnreachedPeerIsOnline(t *testing.T) {
	for name, test := range map[string]struct {
		cached string
		record api.UserMachine
		err    error
		status string
		want   string
	}{
		"cached online, listed offline": {"ready", api.UserMachine{Online: false}, nil, doctor.StatusPass, "peer is offline."},
		"cached offline, listed online": {"offline", api.UserMachine{Online: true}, nil, doctor.StatusFail, "No path to peer"},
		"listed online":                 {"ready", api.UserMachine{Online: true}, nil, doctor.StatusFail, "No path to peer"},
		"removed since":                 {"ready", api.UserMachine{}, &api.APIError{Status: http.StatusGone, Code: "user_machine_removed"}, doctor.StatusPass, "No connection"},
		"no answer, last seen online":   {"ready", api.UserMachine{}, errors.New("timeout"), doctor.StatusFail, "No path to peer"},
		"no answer, last seen offline":  {"offline", api.UserMachine{}, errors.New("timeout"), doctor.StatusPass, "peer is offline."},
	} {
		t.Run(name, func(t *testing.T) {
			var asked []string
			command := commandWithMachineRecord(func(id string) (api.UserMachine, error) {
				asked = append(asked, id)
				return test.record, test.err
			})
			idle := localapi.MachineStatus{ID: "machine_idle", Alias: "idle", RuntimeState: "offline", SelectedPath: "none"}
			snapshot := localapi.Snapshot{DaemonState: "ready", Machines: []localapi.MachineStatus{unreachedPeer("peer", test.cached), idle}}
			ran := false
			for _, probe := range doctorProbes(command, snapshot, nil) {
				if probe.Code != "peer_paths" {
					continue
				}
				ran = true
				if check := probe.Run(context.Background()); check.Status != test.status || !strings.Contains(check.Summary, test.want) {
					t.Fatalf("peer paths check = %+v, want %s with %q", check, test.status, test.want)
				}
			}
			if !ran {
				t.Fatal("no peer_paths probe")
			}
			// Only a machine this computer failed to reach costs a request.
			if !slices.Equal(asked, []string{"machine_peer"}) {
				t.Fatalf("server asked about %v", asked)
			}
			if snapshot.Machines[0].RuntimeState != test.cached {
				t.Fatalf("the daemon snapshot was changed to %q", snapshot.Machines[0].RuntimeState)
			}
		})
	}
}

// The daemon lists the machines of every workspace, so the lookup is not narrowed to the selected one: the server
// answers 404 for a team machine asked for in the personal workspace, and doctor kept the stale "online".
func TestDoctorPeerLookupSeesEveryWorkspace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/machines/machine_team" || r.URL.Query().Has("workspace") {
			http.NotFound(w, r)
			return
		}
		writeAPIData(t, w, map[string]any{"id": "machine_team", "alias": "team", "online": false})
	}))
	defer server.Close()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	writeTestProfile(t, dir, configPath, server.URL)
	t.Setenv("PAPERBOAT_WORKSPACE", "personal")
	command := &cobra.Command{}
	command.Flags().String("config", configPath, "")
	machine, err := doctorMachineRecord(context.Background(), command, "machine_team")
	if err != nil || machine.ID != "machine_team" || machine.Online {
		t.Fatalf("machine = %+v, err = %v", machine, err)
	}
}

// The no_path item lasts 2 minutes, the daemon can list the machine online for 5, and the "no online machine was
// reached" warning reads failures for 10: a machine that went offline must not make doctor degraded in between.
func TestDoctorDoesNotWarnAboutAnOfflinePeerAfterNoPathExpires(t *testing.T) {
	failed := time.Now().Add(-3 * time.Minute)
	peer := localapi.MachineStatus{ID: "machine_peer", Alias: "peer", Eligible: true, RuntimeState: "ready", SelectedPath: "none", LastFailedAt: &failed}
	command := commandWithMachineRecord(func(string) (api.UserMachine, error) { return api.UserMachine{Online: false}, nil })
	for _, probe := range doctorProbes(command, localapi.Snapshot{DaemonState: "ready", Machines: []localapi.MachineStatus{peer}}, nil) {
		if probe.Code == "peer_paths" {
			if check := probe.Run(context.Background()); check.Status != doctor.StatusPass {
				t.Fatalf("peer paths check = %+v", check)
			}
			return
		}
	}
	t.Fatal("no peer_paths probe")
}

// A lookup can outlast its budget (a token refresh keeps its own 30 s timeout): the machines after it are not asked.
func TestDoctorStopsAskingWhenTheLookupBudgetIsSpent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	asked := 0
	doctorServerPresence(ctx, []localapi.MachineStatus{unreachedPeer("a", "ready"), unreachedPeer("b", "ready"), unreachedPeer("c", "ready")}, func(context.Context, string) (api.UserMachine, error) {
		asked++
		cancel()
		return api.UserMachine{}, context.Canceled
	})
	if asked != 1 {
		t.Fatalf("asked %d machines after the budget was spent", asked)
	}
}
