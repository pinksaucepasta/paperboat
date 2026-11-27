package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/pinksaucepasta/paperboat/cli/internal/resolver"
	"github.com/pinksaucepasta/paperboat/cli/internal/tunnel"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
	"github.com/spf13/cobra"
)

type pingSample struct {
	Sequence   int     `json:"sequence"`
	Path       string  `json:"path,omitempty"`
	RTTMS      float64 `json:"rtt_ms,omitempty"`
	Lost       bool    `json:"lost"`
	Transition bool    `json:"path_transition"`
}

type pingReport struct {
	Schema       string       `json:"schema"`
	MachineID    string       `json:"machine_id"`
	MachineName  string       `json:"machine_name"`
	Sent         int          `json:"sent"`
	Received     int          `json:"received"`
	Lost         int          `json:"lost"`
	LossPercent  float64      `json:"loss_percent"`
	MinRTTMS     float64      `json:"min_rtt_ms,omitempty"`
	AverageRTTMS float64      `json:"average_rtt_ms,omitempty"`
	MaxRTTMS     float64      `json:"max_rtt_ms,omitempty"`
	Samples      []pingSample `json:"samples"`
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
	request, err := localapi.NewPeerStreamRequest(target.MachineID, target.Terminal.EnvironmentID, target.MachineGeneration, "health_probe", operation, "local-health-probe", deadline.Add(serverclock.Offset()), 1<<20, nil)
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
	return tunnel.NativeProbe{Path: probe.Path, RTT: time.Duration(probe.RTTNanoseconds)}, nil
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
	prepared, err := preparePing(command.Context(), command.ErrOrStderr(), func(prepareCtx context.Context) (pingPrepared, error) {
		dependencies, err := buildDeps(ctx)
		if err != nil {
			return pingPrepared{}, err
		}
		if dependencies.peerLocal == nil {
			return pingPrepared{}, errors.New("pb ping requires an authenticated Paperboat server")
		}
		machine, err := resolveMachineWarmFirst(prepareCtx, args[0], func() (*api.Client, error) { return backendClient(ctx) })
		if err != nil {
			return pingPrepared{}, err
		}
		if !machine.Online {
			return pingPrepared{}, &api.APIError{Code: "machine_offline", Message: "This machine is offline."}
		}
		if err := requireLocalDaemonService(prepareCtx, dependencies.cfg); err != nil {
			return pingPrepared{}, fmt.Errorf("prepare local peer transport: %w", err)
		}
		return pingPrepared{dependencies: dependencies, machine: machine}, nil
	})
	if err != nil {
		return err
	}
	dependencies, machine := prepared.dependencies, prepared.machine
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
			if !pingLost(pingErr, report.Received > 0) {
				return fmt.Errorf("authenticated ping sample %d: %w", sequence, pingErr)
			}
			report.Lost++
			report.Samples = append(report.Samples, pingSample{Sequence: sequence, Lost: true})
			if !jsonOutput {
				fmt.Fprintf(command.OutOrStdout(), "sample %d: lost\n", sequence)
			}
			continue
		}
		path := pingPath(result.Path)
		selection := path
		transition := previousSelection != "" && previousSelection != selection
		previousSelection = selection
		rttMS := float64(result.RTT) / float64(time.Millisecond)
		report.Received++
		total += result.RTT
		if report.MinRTTMS == 0 || rttMS < report.MinRTTMS {
			report.MinRTTMS = rttMS
		}
		if rttMS > report.MaxRTTMS {
			report.MaxRTTMS = rttMS
		}
		sample := pingSample{Sequence: sequence, Path: path, RTTMS: rttMS, Transition: transition}
		report.Samples = append(report.Samples, sample)
		if !jsonOutput {
			transitionText := ""
			if transition {
				transitionText = " transition"
			}
			fmt.Fprintf(command.OutOrStdout(), "sample %d: path=%s rtt=%.2fms%s\n", sequence, path, sample.RTTMS, transitionText)
		}
	}
	if report.Received > 0 {
		report.AverageRTTMS = float64(total) / float64(time.Millisecond) / float64(report.Received)
	}
	report.LossPercent = float64(report.Lost) * 100 / float64(report.Sent)
	return writePingReport(command.OutOrStdout(), report, jsonOutput)
}

func writePingReport(out io.Writer, report pingReport, jsonOutput bool) error {
	if jsonOutput {
		encoder := json.NewEncoder(out)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(report); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(out, "summary: sent=%d received=%d loss=%.1f%%", report.Sent, report.Received, report.LossPercent)
		if report.Received > 0 {
			fmt.Fprintf(out, " rtt min/avg/max=%.2f/%.2f/%.2fms", report.MinRTTMS, report.AverageRTTMS, report.MaxRTTMS)
		}
		fmt.Fprintln(out)
	}
	if report.Received == 0 {
		return exitCodeError{code: 1}
	}
	return nil
}

// pingLost reports a sample the machine did not answer. After an answer, a machine that stopped answering also fails a
// re-dial: that is loss too, not a command failure.
func pingLost(err error, answered bool) bool {
	var remote *localapi.RemoteError
	if answered && errors.As(err, &remote) && remote.Code == "peer_probe_unavailable" {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
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

type pingPrepared struct {
	dependencies *deps
	machine      api.UserMachine
}

// Before its first sample pb ping resolves the target and makes sure the local
// daemon serves. On Windows that can stall (a cold daemon start, a busy pipe)
// while printing nothing; say so after pingPrepareNotice and give up after
// pingPrepareTimeout even if a step ignores its context.
var (
	pingPrepareNotice  = 3 * time.Second
	pingPrepareTimeout = localDaemonReadyTimeout + 30*time.Second
)

var errPingPrepareTimeout = errors.New("pb ping could not reach the local Paperboat service in time; run `pb doctor`, then retry")

func preparePing(ctx context.Context, notices io.Writer, prepare func(context.Context) (pingPrepared, error)) (pingPrepared, error) {
	prepareCtx, cancel := context.WithTimeout(ctx, pingPrepareTimeout)
	defer cancel()
	type outcome struct {
		prepared pingPrepared
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		prepared, err := prepare(prepareCtx)
		done <- outcome{prepared, err}
	}()
	notice := time.NewTimer(pingPrepareNotice)
	defer notice.Stop()
	for {
		select {
		case result := <-done:
			return result.prepared, result.err
		case <-notice.C:
			fmt.Fprintln(notices, "Still preparing: finding the machine and starting the local Paperboat service...")
		case <-prepareCtx.Done():
			select {
			case result := <-done:
				return result.prepared, result.err
			default:
			}
			if ctx.Err() != nil {
				return pingPrepared{}, ctx.Err()
			}
			return pingPrepared{}, fmt.Errorf("%w (waited %s)", errPingPrepareTimeout, pingPrepareTimeout)
		}
	}
}
