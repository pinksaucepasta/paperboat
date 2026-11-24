package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/pinksaucepasta/paperboat/cli/internal/resolver"
)

type daemonProbeFunc func(context.Context, localapi.PeerStreamRequest) (localapi.PeerProbeResult, error)

func (f daemonProbeFunc) ProbePeer(ctx context.Context, request localapi.PeerStreamRequest) (localapi.PeerProbeResult, error) {
	return f(ctx, request)
}

func TestDaemonProbePreservesTargetAuthorityAndNativeResult(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	target := resolver.ConnectInfo{MachineID: "machine_1", MachineGeneration: 7, Terminal: &resolver.TerminalTarget{EnvironmentID: "environment_1"}}
	client := daemonProbeFunc(func(_ context.Context, request localapi.PeerStreamRequest) (localapi.PeerProbeResult, error) {
		if err := request.Validate(time.Now()); err != nil {
			t.Fatal(err)
		}
		if request.MachineID != "machine_1" || request.MachineGeneration != 7 || request.EnvironmentID != "environment_1" || request.Consumer != "health_probe" {
			t.Fatalf("wrong probe: %+v", request)
		}
		return localapi.PeerProbeResult{Path: "regional_relay", RTTNanoseconds: 100}, nil
	})
	result, err := probeDaemonPeer(ctx, client, target, "ping_1")
	if err != nil || result.Path != "regional_relay" || result.RTT != 100 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	denied := daemonProbeFunc(func(context.Context, localapi.PeerStreamRequest) (localapi.PeerProbeResult, error) {
		return localapi.PeerProbeResult{}, localapi.ErrPermission
	})
	if _, err := probeDaemonPeer(ctx, denied, target, "wait_transport"); !errors.Is(err, localapi.ErrPermission) {
		t.Fatalf("authority error=%v", err)
	}
}

func TestPingCountsAMachineThatStoppedAnsweringAsLost(t *testing.T) {
	redial := &localapi.RemoteError{Code: "peer_probe_unavailable"}
	if !pingLost(redial, true) || pingLost(redial, false) || !pingLost(context.DeadlineExceeded, false) || pingLost(localapi.ErrPermission, true) {
		t.Fatal("wrong lost samples")
	}
}

func TestPingExitsOneWhenEverySampleIsLost(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		for received, want := range []int{1, 0} {
			var out bytes.Buffer
			var exit exitCodeError
			err := writePingReport(&out, pingReport{Sent: 1, Received: received, Lost: 1 - received}, jsonOutput)
			if errors.As(err, &exit); exit.code != want || (want == 0 && err != nil) || out.Len() == 0 {
				t.Fatalf("json=%t received=%d err=%v out=%q", jsonOutput, received, err, out.String())
			}
			if jsonOutput && !json.Valid(out.Bytes()) {
				t.Fatalf("invalid JSON %q", out.String())
			}
		}
	}
}
