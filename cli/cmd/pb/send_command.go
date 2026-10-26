package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/api"
	filetransfer "github.com/pinksaucepasta/paperboat/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/pinksaucepasta/paperboat/internal/serverclock"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func sendCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "send <path>... --to <machine>",
		Short: "Send files to a machine's Paperboat Inbox",
		Args:  commandArgs(cobra.MinimumNArgs(1)),
		RunE:  runSendCommand,
	}
	command.Flags().String("to", "", "destination machine name or ID")
	command.Flags().String("session", "", "terminal session ID for destination context")
	command.Flags().Bool("json", false, "print JSON")
	command.Flags().Bool("unlimited", false, "send any size, only directly or through your own relay; waits while neither is available (default: your account's transfer mode)")
	addSendManagementCommands(command)
	return command
}

func runSendCommand(cobraCommand *cobra.Command, paths []string) error {
	jsonOutput, _ := cobraCommand.Flags().GetBool("json")
	destinationRef, _ := cobraCommand.Flags().GetString("to")
	sessionID, _ := cobraCommand.Flags().GetString("session")
	if sessionID == "" {
		sessionID = strings.TrimSpace(os.Getenv("PAPERBOAT_TERMINAL_SESSION_ID"))
	}
	ctx := actionContext(cobraCommand, paths)
	dependencies, err := buildDeps(ctx)
	if err != nil {
		return err
	}
	client, err := newWorkspaceAPIClient(ctx, dependencies.cfg.ServerURL, dependencies.auth)
	if err != nil {
		return err
	}
	sourceMachineID, err := configuredMachineID()
	if err != nil {
		return err
	}
	if err := requireLocalDaemonService(ctx.Context, dependencies.cfg); err != nil {
		return fmt.Errorf("prepare local peer transport: %w", err)
	}
	var destination api.UserMachine
	if strings.TrimSpace(destinationRef) != "" {
		destination, err = resolveUserMachine(ctx.Context, client, destinationRef)
		if err != nil {
			return friendlyCommandError(err)
		}
	} else {
		var configured api.TransferDestinationDefault
		var defaultErr error
		if strings.TrimSpace(sessionID) != "" {
			configured, defaultErr = client.TerminalSessionTransferDestination(ctx.Context, sessionID)
		}
		if defaultErr == nil && !configured.Configured {
			configured, defaultErr = client.TransferDestinationDefault(ctx.Context)
		}
		if defaultErr != nil {
			return friendlyCommandError(defaultErr)
		}
		if configured.Configured && configured.Machine != nil {
			destination = *configured.Machine
		} else if sessionID != "" {
			eligible, eligibleErr := client.EligibleTerminalSessionTransferDestinations(ctx.Context, sessionID)
			if eligibleErr != nil {
				return friendlyCommandError(eligibleErr)
			}
			eligible = slices.DeleteFunc(eligible, func(machine api.UserMachine) bool { return machine.ID == sourceMachineID })
			switch len(eligible) {
			case 0:
				return localArgumentError("No transfer destination is attached to this terminal session. Name the receiving machine with --to <machine>.")
			case 1:
				destination = eligible[0]
			default:
				if jsonOutput || !term.IsTerminal(int(os.Stdin.Fd())) {
					return localArgumentError("More than one machine can receive this transfer; name one with --to <machine> (`pb machine list` shows them).")
				}
				index, promptErr := chooseIndex(cobraCommand.Context(), "Choose a transfer destination", "Eligible machines for this session", len(eligible), func(index int) selector.Item {
					machine := eligible[index]
					return selector.Item{ID: machine.ID, Title: machine.Alias, Description: preferenceDetails(cobraCommand.Context(), "machines", map[string]string{"status": machineStatusSummary(machine), "platform": machine.Platform, "id": machine.ID}), Search: machineStatusSearch(machine)}
				})
				if promptErr != nil {
					return promptErr
				}
				destination = eligible[index]
			}
		} else {
			return localArgumentError("Name the receiving machine with --to <machine> (or set a default with `pb send destination set <machine>`).")
		}
	}
	if destination.State == "revoked" || destination.State == "disconnected" || destination.State == "deleted" {
		return errors.New("destination machine is revoked")
	}
	if sessionID == "" && !destination.Capabilities.FileReceive.Configured {
		return &api.APIError{Code: "machine_capability_unavailable", Message: "This machine is not configured to receive files."}
	}
	if sessionID == "" && (!destination.Online || !destination.Capabilities.FileReceive.Observed) {
		return &api.APIError{Code: "machine_offline", Message: "The destination machine is offline."}
	}
	if !jsonOutput {
		fmt.Fprintf(cobraCommand.ErrOrStderr(), "Sending to %s (%s)\n", destination.Alias, destination.ID)
	}
	preparedPaths := make([]string, len(paths))
	for i, path := range paths {
		preparedPaths[i], err = filepath.Abs(path)
		if err != nil {
			return err
		}
	}
	prepared, err := filetransfer.Prepare(preparedPaths, automaticTransferLimits)
	if err != nil {
		return err
	}
	defer prepared.Close()
	batchID, err := filetransfer.NewBatchID()
	if err != nil {
		return err
	}
	requestFiles := make([]api.TeamInboxFile, len(prepared.Sources))
	for i, source := range prepared.Sources {
		requestFiles[i] = api.TeamInboxFile{Basename: source.Basename, Size: source.Size, SHA256: hex.EncodeToString(source.SHA256[:])}
	}
	requestUUID, err := uuid.NewRandom()
	if err != nil {
		return err
	}
	requestID := "request_" + requestUUID.String()
	operationID := newIdempotencyKey()
	acceptance, err := client.CreateTeamInboxRequest(ctx.Context, api.TeamInboxRequest{RequestID: requestID, SourceMachineID: sourceMachineID, DestinationMachineID: destination.ID, BatchID: batchID, Files: requestFiles, ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}, operationID)
	if err != nil {
		if limit := serverTransferLimit(err, prepared.Sources); limit != nil {
			return limit
		}
		return friendlyCommandError(err)
	}
	if acceptance.Status == "pending" {
		if !jsonOutput {
			fmt.Fprintf(cobraCommand.ErrOrStderr(), "Waiting for recipient approval (%s).\n", acceptance.RequestID)
		}
		for acceptance.Status == "pending" {
			select {
			case <-ctx.Context.Done():
				return ctx.Context.Err()
			case <-time.After(2 * time.Second):
			}
			acceptance, err = client.TeamInboxRequest(ctx.Context, acceptance.RequestID)
			if err != nil {
				return friendlyCommandError(err)
			}
		}
	}
	if acceptance.Status == "declined" {
		return commandRejection{reason: commandRejectInboxDeclined}
	}
	if acceptance.Status != "approved" && acceptance.Status != "not_required" {
		return fmt.Errorf("file transfer was not accepted: %s", acceptance.Status)
	}
	var batch filetransfer.Batch
	if destination.ID == sourceMachineID {
		// This computer is the destination: the files are copied into its own Inbox, with no transfer.
		batch, err = deliverToThisComputer(batchID, sourceMachineID, destination.ID, destination.OwnerAccount, prepared.Sources)
		if err != nil {
			return err
		}
	} else {
		descriptorRequestID, descriptorDigest := acceptance.RequestID, acceptance.ManifestDigest
		if acceptance.Status == "not_required" {
			descriptorRequestID, descriptorDigest = "", ""
		}
		declared := api.TransferDeclaration{Mode: "automatic", Bytes: batchTotal(prepared.Sources)}
		descriptor, err := client.MachineFileTransferDescriptorForRequest(ctx.Context, destination.ID, sourceMachineID, sessionID, descriptorRequestID, descriptorDigest, declared)
		if err != nil {
			return friendlyCommandError(err)
		}
		target := &resolver.FileTransferTarget{
			Endpoint: descriptor.Endpoint, SourceMachineID: descriptor.SourceMachineID,
			DestinationMachineID: descriptor.DestinationMachineID, InitiatingUserID: descriptor.InitiatingUserID,
			Auth:   resolver.AuthTarget{Method: descriptor.Auth.Method, Token: descriptor.Auth.Token, ExpiresAt: descriptor.Auth.ExpiresAt.UTC().Format(time.RFC3339Nano), ResourceID: descriptor.Auth.AccessSessionID},
			Policy: descriptor.Policy,
		}
		if err := filetransfer.ValidateSources(prepared.Sources, fileTransferLimits(target)); err != nil {
			return err
		}
		if target.Endpoint == "" || target.Auth.Method != "bearer" || target.Auth.Token == "" {
			return errors.New("server returned an invalid file transfer descriptor")
		}
		retention := time.Duration(target.Policy.RetentionSeconds) * time.Second
		if retention <= 0 || retention > 7*24*time.Hour {
			retention = 7 * 24 * time.Hour
		}
		expiresAt := time.Now().UTC().Truncate(time.Second).Add(retention)
		if localSender, localErr := localFileTransferSenderFromEnvironment(); localErr != nil {
			return localErr
		} else if localSender != nil {
			if sessionID == "" {
				return errors.New("host-local file delivery requires a Paperboat terminal session")
			}
			batch, err = localSender.SendNativeBatch(ctx.Context, batchID, sourceMachineID, destination.ID, descriptor.InitiatingUserID, sessionID, prepared.Sources, expiresAt)
		} else {
			if dependencies.peerLocal == nil {
				return errors.New("local daemon transport is unavailable for native file transfer")
			}
			grantForDescriptor := func(grantCtx context.Context, current api.FileTransfer) (filetransfer.NativeGrant, error) {
				if current.Endpoint != descriptor.Endpoint || current.SourceMachineID != descriptor.SourceMachineID || current.DestinationMachineID != descriptor.DestinationMachineID || current.InitiatingUserID != descriptor.InitiatingUserID || current.Policy != descriptor.Policy || current.Auth.Method != "bearer" || current.Auth.Token == "" || !current.Auth.ExpiresAt.After(serverclock.Now()) {
					return filetransfer.NativeGrant{}, errors.New("renewed transfer authorization changed the approved resource or policy")
				}
				lease, leaseErr := dependencies.peerLocal.PrepareFileTransfer(grantCtx, localapi.FileTransferRequest{Schema: localapi.FileTransferSchemaV1, MachineID: destination.ID, EnvironmentID: destination.EnvironmentID, MachineGeneration: uint64(destination.InstallationGeneration), OperationID: batchID, Credential: current.Auth.Token, AccessSessionID: current.Auth.AccessSessionID, Deadline: current.Auth.ExpiresAt, MaximumBytes: transferLeaseBytes(current.Policy, declared), TransferMode: declared.Mode})
				if leaseErr != nil {
					return filetransfer.NativeGrant{}, leaseErr
				}
				return filetransfer.NativeGrant{Endpoint: current.Endpoint, Auth: filetransfer.Auth{Token: current.Auth.Token, ExpiresAt: current.Auth.ExpiresAt}, Binding: filetransfer.Binding{SourceMachineID: current.SourceMachineID, DestinationMachineID: current.DestinationMachineID, InitiatingUserID: current.InitiatingUserID}, Policy: descriptorFileTransferPolicy(&resolver.FileTransferTarget{Policy: current.Policy}), Lease: lease}, nil
			}
			initialGrant, grantErr := grantForDescriptor(ctx.Context, descriptor)
			if grantErr != nil {
				return grantErr
			}
			nativeClient, nativeErr := filetransfer.NewRenewableNativeClient(initialGrant, func(renewCtx context.Context) (filetransfer.NativeGrant, error) {
				fresh, renewErr := client.MachineFileTransferDescriptorForRequest(renewCtx, destination.ID, sourceMachineID, sessionID, descriptorRequestID, descriptorDigest, declared)
				if renewErr != nil {
					return filetransfer.NativeGrant{}, renewErr
				}
				return grantForDescriptor(renewCtx, fresh)
			})
			if nativeErr != nil {
				return errors.Join(nativeErr, initialGrant.Lease.Close())
			}
			if !jsonOutput {
				lastWait := ""
				nativeClient.Waiting = func(message string) {
					if message != lastWait {
						lastWait = message
						fmt.Fprintln(cobraCommand.ErrOrStderr(), message)
					}
				}
			}
			batch, err = nativeClient.SendBatch(ctx.Context, batchID, sessionID, prepared.Sources)
			err = destinationBusy(destination.Alias, destinationStorageFull(destination.Alias, err))
			if closeErr := nativeClient.Close(); closeErr != nil {
				err = errors.Join(err, closeErr)
			}
		}
		if err != nil {
			return err
		}
	}
	if acceptance.Status == "approved" {
		if err := client.CompleteTeamInboxRequest(ctx.Context, acceptance.RequestID, acceptance.ManifestDigest); err != nil {
			reportDeliveredReceiptFailure(ctx.Context, cobraCommand.ErrOrStderr(), jsonOutput, err)
		}
	}
	if jsonOutput {
		return json.NewEncoder(cobraCommand.OutOrStdout()).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": batch})
	}
	for i, item := range batch.Transfers {
		path := item.ReceiptPath
		if i < len(batch.Paths) && batch.Paths[i] != "" {
			path = batch.Paths[i]
		}
		fmt.Fprintf(cobraCommand.OutOrStdout(), "%s: delivered to %s on %s\n", item.Basename, path, destination.Alias)
	}
	return nil
}

// batchTotal is the declared size of a send: every prepared file, directories expanded.
func batchTotal(sources []filetransfer.Source) int64 {
	var total int64
	for _, source := range sources {
		total += source.Size
	}
	return total
}

// transferLeaseBytes bounds the daemon's stream lease for a send: the policy's per-file cap, or the declared
// batch total when larger (an Automatic send up to 2,000,000,000 bytes under today's 50 MiB policy), plus room
// for the transfer's own framing.
func transferLeaseBytes(policy api.FileTransferPolicy, declared api.TransferDeclaration) uint64 {
	limit, _ := policy.Bounds()
	return uint64(max(limit, declared.Bytes)) + 1<<20
}

// unlimitedTransferLimits bound an Unlimited send only by file count: no Paperboat size limit applies.
var unlimitedTransferLimits = filetransfer.Limits{MaxFileBytes: 1 << 40, MaxBatchFiles: 10, MaxBatchBytes: 1 << 40}

// sendTransferMode is --unlimited, else the account's preference, else Automatic.
func sendTransferMode(ctx context.Context, command *cobra.Command, client *api.Client) string {
	if unlimited, _ := command.Flags().GetBool("unlimited"); unlimited {
		return "unlimited"
	}
	if mode, err := client.FileTransferMode(ctx); err == nil {
		return mode
	}
	return "automatic"
}

// declaredTransferLimits widens the descriptor policy's size bounds to what the server signed for this
// declared send (its total for Automatic, none for Unlimited); the policy still carries today's 50 MiB caps.
func declaredTransferLimits(policy filetransfer.Limits, declared api.TransferDeclaration) filetransfer.Limits {
	switch {
	case declared.Mode == "unlimited":
		policy.MaxFileBytes, policy.MaxBatchBytes = unlimitedTransferLimits.MaxFileBytes, unlimitedTransferLimits.MaxBatchBytes
	case declared.Bytes > 0:
		policy.MaxFileBytes, policy.MaxBatchBytes = max(policy.MaxFileBytes, declared.Bytes), max(policy.MaxBatchBytes, declared.Bytes)
	}
	return policy
}
