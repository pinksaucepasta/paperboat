package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
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
		return localapi.PeerProbeResult{Path: "regional_relay", ConnectionNanoseconds: 100}, nil
	})
	result, err := probeDaemonPeer(ctx, client, target, "ping_1")
	if err != nil || result.Path != "regional_relay" || result.Connection != 100 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	denied := daemonProbeFunc(func(context.Context, localapi.PeerStreamRequest) (localapi.PeerProbeResult, error) {
		return localapi.PeerProbeResult{}, localapi.ErrPermission
	})
	if _, err := probeDaemonPeer(ctx, denied, target, "wait_transport"); !errors.Is(err, localapi.ErrPermission) {
		t.Fatalf("authority error=%v", err)
	}
}
