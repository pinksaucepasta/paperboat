package main

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/pinksaucepasta/paperboat/cli/internal/selfupdate"
)

// machineVersionSkewError says a machine's stream refusal comes from its pb build, not the network (X-36). An older
// target answers a newer client's stream header with the same bare reset as a denial, so the daemon's code is only
// "could not reach"; the server knows each machine's running version (update status current_version).
type machineVersionSkewError struct {
	message string
	cause   error
}

func (e *machineVersionSkewError) Error() string { return e.message }
func (e *machineVersionSkewError) Unwrap() error { return e.cause }

type machineVersionReader interface {
	MachineUpdateStatus(ctx context.Context, id string) (map[string]any, error)
}

// explainMachineVersionSkew replaces a peer stream refusal with the skew message when the target reports another pb
// build than this one. Any other error, or an unreadable version, comes back unchanged. workspace is the one the
// stream's header carried: a non-default one is the field an older target rejects, and an exec it rejects ends as an
// uncertain start (the reset arrives after the request was written), so that outcome counts as a refusal then too.
func explainMachineVersionSkew(ctx context.Context, client machineVersionReader, machine api.UserMachine, workspace string, err error) error {
	var already *machineVersionSkewError
	if errors.As(err, &already) {
		return err
	}
	var remote *localapi.RemoteError
	refused := errors.As(err, &remote) && (remote.Code == "peer_transport_unavailable" || remote.Code == "peer_stream_unavailable")
	namedWorkspace := workspace != "" && workspace != "personal"
	if client == nil || !(refused || namedWorkspace && execStartUncertain(err)) {
		return err
	}
	if skew := machineVersionSkew(ctx, client, machine); skew != nil {
		skew.cause = err
		return skew
	}
	return err
}

// machineVersionSkew is the skew message for a machine whose running pb differs from this one, or nil when it is the same,
// newer, or its version cannot be read.
func machineVersionSkew(ctx context.Context, client machineVersionReader, machine api.UserMachine) *machineVersionSkewError {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	status, statusErr := client.MachineUpdateStatus(ctx, machine.ID)
	theirs, _ := status["current_version"].(string)
	ours := buildinfo.Version
	if statusErr != nil || theirs == "" || theirs == ours {
		return nil
	}
	name := machine.Alias
	if name == "" {
		name = machine.ID
	}
	if order, orderErr := selfupdate.CompareVersions(theirs, ours); orderErr == nil {
		if order >= 0 {
			return nil
		}
		return &machineVersionSkewError{message: name + " runs pb " + theirs + ", older than this pb " + ours + "; update it (`pb update` on that machine), then retry."}
	}
	// Development builds carry no order: say both and which side to update.
	return &machineVersionSkewError{message: name + " runs pb " + theirs + " and this pb is " + ours + "; a build older than the other can refuse its connections. Update the older one (`pb update` on that machine), then retry."}
}
