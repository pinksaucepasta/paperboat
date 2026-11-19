package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/pinksaucepasta/paperboat/cli/internal/resolver"
)

func waitTestSnapshot() localapi.Snapshot {
	return localapi.Snapshot{Generation: 3, ObservedAt: time.Unix(1700000000, 0).UTC()}
}

// pb wait on a team machine: the daemon's snapshot does not list it, the server does (as for exec).
func TestWaitResolvesATeamMachineOnTheServerAndUnknownNamesStayNotFound(t *testing.T) {
	server := workspaceMachinesServer(t, map[string][]map[string]any{"laneo-f13-team": {{"id": "machine_pb", "alias": "laneo-pb"}}})
	client := api.New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	if err := client.SetWorkspace("personal"); err != nil {
		t.Fatal(err)
	}
	machine, backend, err := resolveWaitMachineOn(context.Background(), client, "laneo-pb")
	if err != nil || backend == nil || machine.ID != "machine_pb" || backend.Workspace() != "laneo-f13-team" {
		t.Fatalf("team machine: machine=%+v backend=%v err=%v", machine, backend, err)
	}
	if _, _, err := resolveWaitMachineOn(context.Background(), client, "nowhere"); !errors.Is(err, resolver.ErrMachineNotFound) {
		t.Fatalf("unknown name: %v", err)
	}
}

func fastServerPoll() context.Context {
	return withTimings(context.Background(), func(timings *commandTimings) { timings.waitServerPoll = time.Millisecond })
}

func TestWaitOnATeamMachineReturnsWhenTheServerSeesItReady(t *testing.T) {
	offline := api.UserMachine{ID: "machine_pb", Alias: "laneo-pb", Ownership: "team"}
	polls := 0
	refresh := func() (api.UserMachine, error) {
		polls++
		if polls < 3 {
			return offline, nil
		}
		ready := offline
		ready.Online, ready.InstallationGeneration = true, 4
		return ready, nil
	}
	ctx, cancel := context.WithTimeout(fastServerPoll(), 5*time.Second)
	defer cancel()
	result, err := waitForServerMachineWith(ctx, "laneo-f13-team", waitTestSnapshot(), offline, "runtime", nil, refresh)
	if err != nil || result.Outcome != "ready" || result.Machine.Generation != 4 || result.Validate() != nil || polls != 3 {
		t.Fatalf("result=%+v err=%v polls=%d validate=%v", result, err, polls, result.Validate())
	}
}

func TestWaitOnATeamMachineTransportUsesThePeerProbeInItsWorkspace(t *testing.T) {
	ready := api.UserMachine{ID: "machine_pb", EnvironmentID: "environment_pb", Alias: "laneo-pb", Online: true, InstallationGeneration: 4}
	var workspace string
	prober := daemonProbeFunc(func(_ context.Context, request localapi.PeerStreamRequest) (localapi.PeerProbeResult, error) {
		workspace = request.Workspace
		return localapi.PeerProbeResult{Path: "direct"}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := waitForServerMachineWith(ctx, "laneo-f13-team", waitTestSnapshot(), ready, "transport", prober, nil)
	if err != nil || result.Outcome != "ready" || result.Machine.SelectedPath != "direct" || workspace != "laneo-f13-team" || result.Validate() != nil {
		t.Fatalf("result=%+v err=%v workspace=%q", result, err, workspace)
	}
}

func TestWaitOnATeamMachineEndsOnRemovalAndOnTheDeadline(t *testing.T) {
	offline := api.UserMachine{ID: "machine_pb", Alias: "laneo-pb"}
	ctx, cancel := context.WithTimeout(fastServerPoll(), 5*time.Second)
	defer cancel()
	removed, err := waitForServerMachineWith(ctx, "", waitTestSnapshot(), offline, "runtime", nil, func() (api.UserMachine, error) { return api.UserMachine{}, resolver.ErrMachineNotFound })
	if err != nil || removed.Outcome != "failed" || removed.Code != "machine_removed" || removed.Validate() != nil {
		t.Fatalf("removed: %+v err=%v", removed, err)
	}
	short, cancelShort := context.WithTimeout(fastServerPoll(), 20*time.Millisecond)
	defer cancelShort()
	timedOut, err := waitForServerMachineWith(short, "", waitTestSnapshot(), offline, "runtime", nil, func() (api.UserMachine, error) { return offline, nil })
	if err != nil || timedOut.Outcome != "timeout" || timedOut.Validate() != nil || waitExitCode(timedOut) != 203 {
		t.Fatalf("timeout: %+v err=%v", timedOut, err)
	}
}
