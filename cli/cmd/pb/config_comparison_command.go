package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"github.com/spf13/cobra"
)

type configComparisonFailure struct{ cause error }

func (e *configComparisonFailure) Error() string {
	return "Both conflict versions could not be read. No resolution was applied. Refresh `pb config status` and retry; run `pb doctor` if the encrypted peer connection remains unavailable."
}
func (e *configComparisonFailure) Unwrap() error { return e.cause }
func configConflictCompare(c *command.Context) (resultErr error) {
	reading := false
	defer func() {
		if reading && resultErr != nil && !errors.Is(resultErr, context.Canceled) {
			resultErr = &configComparisonFailure{cause: resultErr}
		}
	}()
	client, _, profile, err := e2eeClient(c)
	if err != nil {
		return err
	}
	_, items, err := loadSelectedConfigStatus(c, c.Args().First())
	if err != nil {
		return err
	}
	state := items[0]
	conflict, err := findConfigConflict(state, c.Args().Get(1))
	if err != nil {
		return err
	}
	source, err := configuredMachineID()
	if err != nil {
		return err
	}
	descriptor, err := client.ConfigConflictConnection(c.Context, state.EnvironmentID, conflict.Revision, state.AssignmentVersion, conflict.Path, state.RemoteRevision, source, profile.CLIClientSessionID)
	if err != nil {
		return friendlyCommandError(err)
	}
	if descriptor.Environment.ResourceID != state.MachineID {
		return errors.New("comparison target changed; refresh config status")
	}
	machines, err := client.ListUserMachines(c.Context)
	if err != nil {
		return err
	}
	var generation uint64
	for _, machine := range machines {
		if machine.ID == state.MachineID && machine.InstallationGeneration > 0 {
			generation = uint64(machine.InstallationGeneration)
		}
	}
	if generation == 0 {
		return errors.New("comparison target is no longer available; refresh config status")
	}
	parent, ok := c.Context.Value(confirmationCommandKey{}).(*cobra.Command)
	if !ok {
		return errors.New("comparison requires CLI command context")
	}
	local, _, err := localDaemonSnapshot(parent, installLocalDaemonService)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(descriptor.Binding)
	request, err := localapi.NewPeerStreamRequest(state.MachineID, state.EnvironmentID, generation, "config_compare", descriptor.OperationID, descriptor.Auth.Token, descriptor.ExpiresAt, 220<<20, payload)
	if err != nil {
		return err
	}
	request.AccessSessionID = descriptor.Binding.AssignmentID
	request.UsageSessionID = descriptor.Auth.AccessSessionID
	reading = true
	stream, err := local.OpenPeerStream(c.Context, request)
	if err != nil {
		return err
	}
	defer func() {
		if err := stream.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	reader := bufio.NewReaderSize(stream, protocol.MaxStructuredFrame+1)
	line, err := reader.ReadSlice('\n')
	if err != nil || len(line) > protocol.MaxStructuredFrame {
		return errors.New("comparison metadata unavailable")
	}
	var metadata tunnel.ComparisonMetadata
	if json.Unmarshal(line, &metadata) != nil || metadata.ConflictComparisonRequest != descriptor.Binding || metadata.Local.Bytes < 0 || metadata.Managed.Bytes < 0 || metadata.Local.Bytes > tunnel.MaxComparisonSideBytes || metadata.Managed.Bytes > tunnel.MaxComparisonSideBytes {
		return errors.New("invalid comparison metadata")
	}
	comparison := configsync.ConflictComparison{ConflictComparisonRequest: descriptor.Binding, Local: configsync.ConflictComparisonSide{Present: metadata.Local.Present, Kind: metadata.Local.Kind, SHA256: metadata.Local.SHA256}, Managed: configsync.ConflictComparisonSide{Present: metadata.Managed.Present, Kind: metadata.Managed.Kind, SHA256: metadata.Managed.SHA256}}
	defer func() { clear(comparison.Local.Content); clear(comparison.Managed.Content) }()
	for {
		frame, err := protocol.ReadBinaryFrame(reader)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		side := &comparison.Local
		max := metadata.Local.Bytes
		if frame.Channel == protocol.Stderr {
			side = &comparison.Managed
			max = metadata.Managed.Bytes
		} else if frame.Channel != protocol.Stdout {
			return errors.New("invalid comparison channel")
		}
		if frame.StartSequence != uint64(len(side.Content)) || int64(len(side.Content)+len(frame.Data)) > max {
			return errors.New("invalid comparison byte sequence")
		}
		side.Content = append(side.Content, frame.Data...)
	}
	if !tunnel.VerifyComparisonSide(metadata.Local, comparison.Local.Content) || !tunnel.VerifyComparisonSide(metadata.Managed, comparison.Managed.Content) {
		return errors.New("comparison integrity failed; refresh and retry")
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(comparison)
	}
	fmt.Fprintf(c.Writer, "%s — this machine (%s)\n", conflict.Path, comparison.Local.Kind)
	if _, err = c.Writer.Write(comparison.Local.Content); err != nil {
		return err
	}
	fmt.Fprintf(c.Writer, "\n%s — repository (%s)\n", conflict.Path, comparison.Managed.Kind)
	if _, err = c.Writer.Write(comparison.Managed.Content); err != nil {
		return err
	}
	fmt.Fprintln(c.Writer)
	return nil
}
