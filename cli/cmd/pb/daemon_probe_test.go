package main

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/connectionmanager"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"testing"
	"time"
)

type daemonProbeFunc func(context.Context, localapi.PeerStreamRequest) (localapi.PeerProbeResult, error)

func (f daemonProbeFunc) ProbePeer(ctx context.Context, request localapi.PeerStreamRequest) (localapi.PeerProbeResult, error) {
	return f(ctx, request)
}

func TestDaemonProbePreservesTargetAuthorityAndResults(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	target := resolver.ConnectInfo{ProjectID: "machine_1", MachineGeneration: 7, Terminal: &resolver.TerminalTarget{EnvironmentID: "environment_1"}}
	client := daemonProbeFunc(func(ctx context.Context, request localapi.PeerStreamRequest) (localapi.PeerProbeResult, error) {
		if err := request.Validate(time.Now()); err != nil {
			t.Fatal(err)
		}
		if request.MachineID != "machine_1" || request.MachineGeneration != 7 || request.EnvironmentID != "environment_1" || request.Transport != "q" || request.Consumer != "health_probe" {
			t.Fatalf("wrong probe: %+v", request)
		}
		return localapi.PeerProbeResult{Transport: "relay_quic", RelayRegion: "fra", RTTNanoseconds: 42, ConnectionNanoseconds: 100, PTOs: 2}, nil
	})
	result, err := probeDaemonPeer(ctx, client, target, "q", "ping_1")
	if err != nil || result.Path != connectionmanager.PathRelayQUIC || result.RelayRegion != "fra" || result.RTT != 42 || result.Connection != 100 || result.PTOs != 2 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	denied := daemonProbeFunc(func(context.Context, localapi.PeerStreamRequest) (localapi.PeerProbeResult, error) {
		return localapi.PeerProbeResult{}, localapi.ErrPermission
	})
	if _, err := probeDaemonPeer(ctx, denied, target, "a", "wait_transport"); !errors.Is(err, localapi.ErrPermission) {
		t.Fatalf("authority error=%v", err)
	}
}

func TestDaemonPathProbesKeepIndependentOutcomes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	target := resolver.ConnectInfo{ProjectID: "machine_1", MachineGeneration: 7, Terminal: &resolver.TerminalTarget{EnvironmentID: "environment_1"}}
	client := daemonProbeFunc(func(_ context.Context, request localapi.PeerStreamRequest) (localapi.PeerProbeResult, error) {
		if err := request.Validate(time.Now()); err != nil {
			return localapi.PeerProbeResult{}, err
		}
		switch request.Transport {
		case "d":
			return localapi.PeerProbeResult{Transport: "direct_quic", RTTNanoseconds: 1}, nil
		case "q":
			return localapi.PeerProbeResult{}, context.DeadlineExceeded
		case "w":
			return localapi.PeerProbeResult{Transport: "wss", RTTNanoseconds: 3}, nil
		default:
			return localapi.PeerProbeResult{}, errors.New("unexpected transport")
		}
	})
	results := probeDaemonPaths(ctx, client, target)
	if len(results) != 3 || !results[connectionmanager.PathDirectQUIC].Reachable || results[connectionmanager.PathRelayQUIC].Reachable || !results[connectionmanager.PathWSS].Reachable {
		t.Fatalf("paths=%+v", results)
	}
}
