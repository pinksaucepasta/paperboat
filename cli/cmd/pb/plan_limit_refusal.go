package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// planLimitRefusal surfaces the plan limit when it ended a command mid-way. A renewal of a transfer,
// exec or ssh authorization refused at the limit is wrapped with the transport error it caused (a
// closed stream). A connection the withheld relay could not carry, or a stream the relay cut at the
// limit, carries no refusal at all, so the daemon's snapshot, which holds the latest relay refusal, is
// asked.
// Otherwise the command would report only a connectivity failure without the limit and Billing link.
func planLimitRefusal(ctx context.Context, err error) error {
	var refused *api.APIError
	if errors.As(err, &refused) && refused.Code == localapi.RelayQuotaCode {
		return refused
	}
	if !connectionFailureKind(ctx, classifyCommandFailure(err).kind) {
		return err
	}
	if message := daemonRelayDenial(ctx); message != "" {
		return &api.APIError{Status: http.StatusConflict, Code: localapi.RelayQuotaCode, Message: message}
	}
	return err
}

// connectionFailureKind reports whether a failure may be a peer connection that needed the withheld
// relay: exec, ssh, send and terminal end as a lost connection, a deadline, an unexpected local service
// failure, or a cancellation that did not come from the user (the command's own context is still live).
func connectionFailureKind(ctx context.Context, kind commandFailureKind) bool {
	switch kind {
	case commandOperational, commandDeadline, commandUnexpected:
		return true
	case commandCanceled:
		return ctx.Err() == nil
	}
	return false
}

// relayDenialLookupTimeout bounds the extra daemon read on an already failed command.
const relayDenialLookupTimeout = 2 * time.Second

var daemonSnapshotForPlanLimit = func(ctx context.Context) (localapi.Snapshot, error) {
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		return localapi.Snapshot{}, err
	}
	client, err := localapi.NewClient(paths.SocketPath, relayDenialLookupTimeout)
	if err != nil {
		return localapi.Snapshot{}, err
	}
	return client.Snapshot(ctx)
}

func daemonRelayDenial(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), relayDenialLookupTimeout)
	defer cancel()
	snapshot, err := daemonSnapshotForPlanLimit(ctx)
	if err != nil {
		return ""
	}
	for _, item := range snapshot.Health {
		if item.Code == localapi.RelayQuotaCode {
			return item.Title
		}
	}
	return ""
}
