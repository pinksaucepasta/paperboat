package main

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/preview"
)

// commandTimings holds the waits, polls and deadlines a command may shorten under test. They travel in the command's
// context, never in package variables: a test shortens its own run and nothing another goroutine or test reads.
// timingsOf is the one owner of the production values.
type commandTimings struct {
	// How long a command that has not started its work waits for a machine the server calls offline, and how often it
	// asks again. A host restarting its runtime (pb update) is reported unobserved or offline for a few seconds; a test VM
	// update checks failed sends with machine_offline for the whole network handover. The wait only covers the offline
	// answer: any other error returns at once, and a machine that stays offline reports it when the grace ends.
	machineBlipGrace    time.Duration
	machineBlipInterval time.Duration
	// execReattachAttempt bounds one exec reattach attempt; the daemon's own peer open takes up to 10 s.
	execReattachAttempt time.Duration
	// execReattachWindow bounds one recovery: it outlasts a machine restarting for an update (a Windows worker was
	// unavailable 41 and 48 s through pb install on a Windows test VM; 40 s dropped the exec 2 s before it was back).
	// Attempts alone do not bound time: 40 attempts that each hang to 15 s take 10 min.
	execReattachWindow time.Duration
	// tunnelOperationPoll is how often `--wait` on a tunnel operation or domain reads it again.
	tunnelOperationPoll time.Duration
	// tunnelRequestRetryDelay is the pause before retry number attempt+1 of a control-plane request. Retries are
	// deliberately short and bounded because commands must remain responsive while still recovering ordinary transient
	// failures.
	tunnelRequestRetryDelay func(attempt int) time.Duration
	// waitServerPoll is how often a machine the daemon does not track is looked up on the server again.
	waitServerPoll time.Duration
	// teamCreatePoll is how often `pb team create` asks whether the paid Team exists.
	teamCreatePoll time.Duration
	// doctorClock bounds each of the (at most two) unauthenticated server requests that read the server's clock; both
	// and the pause between them stay inside the probe's 10 s deadline.
	doctorClock time.Duration
	// updateProgress is how often the human update output says it is still working; updateCancelGrace is how long a
	// canceled or expired update waits for the control call to report its own cause.
	updateProgress    time.Duration
	updateCancelGrace time.Duration
	// accessTunnelHeartbeat: hostd closes the listener of a session that has not beaten within its TTL (a CLI killed
	// without Release); both numbers live in preview.
	accessTunnelHeartbeat time.Duration
	// logoutRevocation bounds the server revocation after local sign-out. A server that answers in a few seconds (a slow
	// link) must still revoke: with 2 s, `pb logout` left the session live on the server, and a running exec or terminal
	// kept going until the next login drained the queued revocation.
	logoutRevocation time.Duration
	// renewLimitFloor is the shortest wait after a 429 on a transfer renewal.
	renewLimitFloor time.Duration
	// A destination takes 2 transfers at once and refuses a third at once; a send queues behind them instead (F21-166: 6
	// parallel sends to one machine all finish): it asks again every sendQueuePoll, says so once, and gives up with the
	// busy refusal after sendQueueWait. Nothing is delivered by a refused attempt, so asking again is safe.
	sendQueueWait time.Duration
	sendQueuePoll time.Duration
	// setupOpenSSH bounds the OpenSSH step like an install or pair does: the package may only be reachable through slow
	// fallbacks, and setup must still finish without managed SSH.
	setupOpenSSH time.Duration
	// windowsHandoff bounds the uninstall helper's startup acknowledgement, not the five-minute teardown itself.
	windowsHandoff time.Duration
	// tunnelDoctorProbe bounds the one loopback health request of `pb tunnel doctor`.
	tunnelDoctorProbe time.Duration
}

func defaultCommandTimings() commandTimings {
	return commandTimings{
		machineBlipGrace:        15 * time.Second,
		machineBlipInterval:     150 * time.Millisecond,
		execReattachAttempt:     15 * time.Second,
		execReattachWindow:      90 * time.Second,
		tunnelOperationPoll:     500 * time.Millisecond,
		tunnelRequestRetryDelay: tunnelRequestJitteredDelay,
		waitServerPoll:          3 * time.Second,
		teamCreatePoll:          time.Duration(config.DefaultPollIntervalSeconds) * time.Second,
		doctorClock:             4 * time.Second,
		updateProgress:          5 * time.Second,
		updateCancelGrace:       2 * time.Second,
		accessTunnelHeartbeat:   preview.PrivateTCPAccessHeartbeatInterval,
		logoutRevocation:        10 * time.Second,
		renewLimitFloor:         2 * time.Second,
		sendQueueWait:           15 * time.Minute,
		sendQueuePoll:           3 * time.Second,
		setupOpenSSH:            75 * time.Second,
		windowsHandoff:          30 * time.Second,
		tunnelDoctorProbe:       2 * time.Second,
	}
}

type commandTimingsKey struct{}

// withTimings returns ctx with the production timings changed by shorten.
func withTimings(ctx context.Context, shorten func(*commandTimings)) context.Context {
	timings := timingsOf(ctx)
	shorten(&timings)
	return context.WithValue(ctx, commandTimingsKey{}, timings)
}

func timingsOf(ctx context.Context) commandTimings {
	if timings, ok := ctx.Value(commandTimingsKey{}).(commandTimings); ok {
		return timings
	}
	return defaultCommandTimings()
}
