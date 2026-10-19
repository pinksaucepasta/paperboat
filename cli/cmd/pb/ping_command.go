package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"github.com/spf13/cobra"
)

type pingSample struct {
	Sequence     int     `json:"sequence"`
	Path         string  `json:"path,omitempty"`
	ConnectionMS float64 `json:"connection_ms,omitempty"`
	Lost         bool    `json:"lost"`
	Transition   bool    `json:"path_transition"`
}

type pingReport struct {
	Schema              string       `json:"schema"`
	MachineID           string       `json:"machine_id"`
	MachineName         string       `json:"machine_name"`
	Sent                int          `json:"sent"`
	Received            int          `json:"received"`
	Lost                int          `json:"lost"`
	LossPercent         float64      `json:"loss_percent"`
	MinConnectionMS     float64      `json:"min_connection_ms,omitempty"`
	AverageConnectionMS float64      `json:"average_connection_ms,omitempty"`
	MaxConnectionMS     float64      `json:"max_connection_ms,omitempty"`
	Samples             []pingSample `json:"samples"`
}

// peerProber is the existing authenticated daemon probe boundary.
type peerProber interface {
	ProbePeer(context.Context, localapi.PeerStreamRequest) (localapi.PeerProbeResult, error)
}

func probeDaemonPeer(ctx context.Context, client peerProber, target resolver.ConnectInfo, operation string) (tunnel.NativeProbe, error) {
	if client == nil || target.Terminal == nil {
		return tunnel.NativeProbe{}, errors.New("authenticated daemon probe is unavailable")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return tunnel.NativeProbe{}, errors.New("daemon probe requires a deadline")
	}
	request, err := localapi.NewPeerStreamRequest(target.MachineID, target.Terminal.EnvironmentID, target.MachineGeneration, "health_probe", operation, "local-health-probe", deadline, 1<<20, nil)
	if err != nil {
		return tunnel.NativeProbe{}, err
	}
	if target.Workspace != "" {
		request.Workspace = target.Workspace
	}
	probe, err := client.ProbePeer(ctx, request)
	if err != nil {
		return tunnel.NativeProbe{}, err
	}
	return tunnel.NativeProbe{Path: probe.Path, Connection: time.Duration(probe.ConnectionNanoseconds)}, nil
}

func actionPing(command *cobra.Command, args []string) error {
	count, _ := command.Flags().GetInt("count")
	timeout, _ := command.Flags().GetDuration("timeout")
	jsonOutput, _ := command.Flags().GetBool("json")
	if count < 1 || count > 100 {
		return invocationError(errors.New("--count must be between 1 and 100"))
	}
	if timeout <= 0 || timeout > time.Minute {
		return invocationError(errors.New("--timeout must be greater than zero and no more than 1m"))
	}
	ctx := actionContext(command, args)
	dependencies, err := buildDeps(ctx)
	if err != nil {
		return err
	}
	if dependencies.peerLocal == nil {
		return errors.New("pb ping requires an authenticated Paperboat server")
	}
	client, err := backendClient(ctx)
	if err != nil {
		return err
	}
	machine, err := resolveUserMachine(command.Context(), client, args[0])
	if err != nil {
		return err
	}
	if !machine.Online {
		return &api.APIError{Code: "machine_offline", Message: "This machine is offline."}
	}
	if err := requireLocalDaemonService(command.Context(), dependencies.cfg); err != nil {
		return fmt.Errorf("prepare local peer transport: %w", err)
	}
	target := resolver.ConnectInfo{TargetKind: "machine", MachineID: machine.ID, Machine: machine.Alias, MachineGeneration: uint64(machine.InstallationGeneration), Terminal: &resolver.TerminalTarget{Protocol: "paperboat.health-probe.v1", EnvironmentID: machine.EnvironmentID}}
	report := pingReport{Schema: "paperboat.ping/v1", MachineID: machine.ID, MachineName: machine.Alias, Sent: count, Samples: make([]pingSample, 0, count)}
	previousSelection := ""
	var total time.Duration
	for sequence := 1; sequence <= count; sequence++ {
		sampleCtx, cancel := context.WithTimeout(command.Context(), timeout)
		result, pingErr := probeDaemonPeer(sampleCtx, dependencies.peerLocal, target, newIdempotencyKey())
		cancel()
		if pingErr != nil {
			if command.Context().Err() != nil {
				return command.Context().Err()
			}
			if !errors.Is(pingErr, context.DeadlineExceeded) && !errors.Is(pingErr, context.Canceled) {
				return fmt.Errorf("authenticated ping sample %d: %w", sequence, pingErr)
			}
			report.Lost++
			report.Samples = append(report.Samples, pingSample{Sequence: sequence, Lost: true})
			if !jsonOutput {
				fmt.Fprintf(command.OutOrStdout(), "sample %d: timeout\n", sequence)
			}
			continue
		}
		path := pingPath(result.Path)
		selection := path
		transition := previousSelection != "" && previousSelection != selection
		previousSelection = selection
		connectionMS := float64(result.Connection) / float64(time.Millisecond)
		report.Received++
		total += result.Connection
		if report.MinConnectionMS == 0 || connectionMS < report.MinConnectionMS {
			report.MinConnectionMS = connectionMS
		}
		if connectionMS > report.MaxConnectionMS {
			report.MaxConnectionMS = connectionMS
		}
		sample := pingSample{Sequence: sequence, Path: path, ConnectionMS: connectionMS, Transition: transition}
		report.Samples = append(report.Samples, sample)
		if !jsonOutput {
			transitionText := ""
			if transition {
				transitionText = " transition"
			}
			fmt.Fprintf(command.OutOrStdout(), "sample %d: path=%s connect=%.2fms%s\n", sequence, path, sample.ConnectionMS, transitionText)
		}
	}
	if report.Received > 0 {
		report.AverageConnectionMS = float64(total) / float64(time.Millisecond) / float64(report.Received)
	}
	report.LossPercent = float64(report.Lost) * 100 / float64(report.Sent)
	if jsonOutput {
		encoder := json.NewEncoder(command.OutOrStdout())
		encoder.SetEscapeHTML(false)
		return encoder.Encode(report)
	}
	fmt.Fprintf(command.OutOrStdout(), "summary: sent=%d received=%d loss=%.1f%%", report.Sent, report.Received, report.LossPercent)
	if report.Received > 0 {
		fmt.Fprintf(command.OutOrStdout(), " connect min/avg/max=%.2f/%.2f/%.2fms", report.MinConnectionMS, report.AverageConnectionMS, report.MaxConnectionMS)
	}
	fmt.Fprintln(command.OutOrStdout())
	if report.Received == 0 {
		return exitCodeError{code: 1}
	}
	return nil
}

func pingPath(path string) string {
	switch path {
	case "direct", "peer_relay", "regional_relay", "unknown":
		return path
	default:
		return "unknown"
	}
}

func nativeSnapshotPath(path string) string {
	switch path {
	case "direct":
		return "direct"
	case "peer_relay", "regional_relay":
		return "relay"
	default:
		return ""
	}
}
