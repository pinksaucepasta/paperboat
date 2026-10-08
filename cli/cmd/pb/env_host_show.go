package main

import (
	"encoding/json"
	"fmt"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/spf13/cobra"
)

type vaultHostMetadata struct {
	MachineID              string                          `json:"machine_id"`
	PublicationState       string                          `json:"publication_state"`
	Published              bool                            `json:"published"`
	Applied                bool                            `json:"applied"`
	InstallationGeneration uint64                          `json:"installation_generation"`
	HostKeyGeneration      uint64                          `json:"host_key_generation"`
	SelectionGeneration    uint64                          `json:"selection_generation"`
	ProjectionRevision     uint64                          `json:"projection_revision"`
	DocumentID             string                          `json:"document_id"`
	FenceGeneration        uint64                          `json:"fence_generation"`
	SelectionCount         int                             `json:"selection_count"`
	Observation            *api.VaultProjectionObservation `json:"observation"`
}

func hostDeliveryMetadata(host api.VaultHostState) vaultHostMetadata {
	b := host.Bundle
	return vaultHostMetadata{MachineID: b.MachineID, PublicationState: b.State, Published: b.State == "ready" && b.ProjectionRevision > 0, Applied: host.Applied, InstallationGeneration: b.InstallationGeneration, HostKeyGeneration: b.HostKeyGeneration, SelectionGeneration: b.SelectionGeneration, ProjectionRevision: b.ProjectionRevision, DocumentID: b.DocumentID, FenceGeneration: b.FenceGeneration, SelectionCount: len(host.Selection), Observation: host.Observation}
}
func runVaultHostShow(command *cobra.Command, requestedMachine string) error {
	client, err := environmentVariableBackendForCommand(command)
	if err != nil {
		return err
	}
	target, err := vaultHostTargetForCommand(command, client, requestedMachine)
	if err != nil {
		return err
	}
	host, err := client.GetVaultHost(command.Context(), target.machineID)
	if err != nil {
		return err
	}
	metadata := hostDeliveryMetadata(host)
	if jsonOutputRequested(command) {
		return json.NewEncoder(command.OutOrStdout()).Encode(metadata)
	}
	_, err = fmt.Fprintf(command.OutOrStdout(), "MACHINE\t%s\nPUBLICATION\t%s\nPUBLISHED\t%t\nAPPLIED\t%t\nREVISION\t%d\nDOCUMENT\t%s\nFENCE\t%d\nSELECTIONS\t%d\n", metadata.MachineID, metadata.PublicationState, metadata.Published, metadata.Applied, metadata.ProjectionRevision, metadata.DocumentID, metadata.FenceGeneration, metadata.SelectionCount)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintln(command.OutOrStdout(), "Applied delivery affects new processes; running processes retain their existing environment."); err != nil {
		return err
	}
	if metadata.Observation == nil {
		_, err = fmt.Fprintln(command.OutOrStdout(), "OBSERVATION\tnot reported")
		return err
	}
	observation := metadata.Observation
	_, err = fmt.Fprintf(command.OutOrStdout(), "OBSERVATION\t%s\nOBSERVED AT\t%s\nOBSERVED FENCE\t%d\n", observation.State, observation.ObservedAt.Format("2006-01-02T15:04:05Z07:00"), observation.FenceGeneration)
	if err != nil {
		return err
	}
	if observation.Projection != nil {
		_, err = fmt.Fprintf(command.OutOrStdout(), "OBSERVED REVISION\t%d\nOBSERVED DOCUMENT\t%s\n", observation.Projection.Revision, observation.Projection.DocumentID)
	}
	if err == nil && !metadata.Applied {
		_, err = fmt.Fprintln(command.OutOrStdout(), "The host has not reported applying this exact delivery and fence.")
	}
	return err
}

func vaultHostTargetForCommand(command *cobra.Command, client *api.Client, requestedMachine string) (environmentVariableTarget, error) {
	if requestedMachine == "" {
		var err error
		requestedMachine, err = configuredMachineID()
		if err != nil {
			return environmentVariableTarget{}, err
		}
	}
	// A selected Team source does not transfer ownership of a recipient host.
	if err := client.SetWorkspace("personal"); err != nil {
		return environmentVariableTarget{}, err
	}
	return environmentVariableTargetForCommand(command, client, requestedMachine)
}
