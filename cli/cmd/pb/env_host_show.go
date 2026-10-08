package main

import (
	"encoding/json"
	"fmt"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/spf13/cobra"
)

type vaultLayerMetadata struct {
	Source             api.VaultLayerSource       `json:"source"`
	State              string                     `json:"state"`
	Published          bool                       `json:"published"`
	Applied            bool                       `json:"applied"`
	DeliveryGeneration uint64                     `json:"delivery_generation"`
	DocumentID         string                     `json:"document_id"`
	FenceGeneration    uint64                     `json:"fence_generation"`
	Observation        *api.VaultLayerObservation `json:"observation"`
}
type vaultHostMetadata struct {
	WorkspaceID    string               `json:"workspace_id"`
	ActorAccountID string               `json:"actor_account_id"`
	MachineID      string               `json:"machine_id"`
	Published      bool                 `json:"published"`
	Applied        bool                 `json:"applied"`
	Layers         []vaultLayerMetadata `json:"layers"`
}

func hostDeliveryMetadata(context api.VaultLayerContext) vaultHostMetadata {
	out := vaultHostMetadata{WorkspaceID: context.WorkspaceID, ActorAccountID: context.ActorAccountID, MachineID: context.MachineID, Published: len(context.Layers) > 0, Applied: len(context.Layers) > 0, Layers: []vaultLayerMetadata{}}
	for _, layer := range context.Layers {
		published := layer.State == "ready" && layer.Recipient.DeliveryGeneration > 0
		out.Published = out.Published && published
		out.Applied = out.Applied && layer.Applied
		out.Layers = append(out.Layers, vaultLayerMetadata{Source: layer.Source, State: layer.State, Published: published, Applied: layer.Applied, DeliveryGeneration: layer.Recipient.DeliveryGeneration, DocumentID: layer.Recipient.DocumentID, FenceGeneration: layer.Recipient.FenceGeneration, Observation: layer.Observation})
	}
	return out
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
	host, err := client.GetVaultLayerContext(command.Context(), target.machineID)
	if err != nil {
		return err
	}
	metadata := hostDeliveryMetadata(host)
	if jsonOutputRequested(command) {
		return json.NewEncoder(command.OutOrStdout()).Encode(metadata)
	}
	if _, err := fmt.Fprintf(command.OutOrStdout(), "MACHINE\t%s\nWORKSPACE\t%s\nPUBLISHED\t%t\nAPPLIED\t%t\n", metadata.MachineID, metadata.WorkspaceID, metadata.Published, metadata.Applied); err != nil {
		return err
	}
	for _, layer := range metadata.Layers {
		if _, err := fmt.Fprintf(command.OutOrStdout(), "LAYER\t%s/%s/%s\nSTATE\t%s\nPUBLISHED\t%t\nAPPLIED\t%t\nREVISION\t%d\nDOCUMENT\t%s\nFENCE\t%d\n", layer.Source.OwnerKind, layer.Source.OwnerID, layer.Source.MachineID, layer.State, layer.Published, layer.Applied, layer.Source.Revision, layer.DocumentID, layer.FenceGeneration); err != nil {
			return err
		}
		if layer.Observation == nil {
			if _, err := fmt.Fprintln(command.OutOrStdout(), "OBSERVATION\tnot reported"); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintf(command.OutOrStdout(), "OBSERVATION\t%s\nOBSERVED AT\t%s\n", layer.Observation.State, layer.Observation.ObservedAt.Format("2006-01-02T15:04:05Z07:00")); err != nil {
				return err
			}
		}
	}
	_, err = fmt.Fprintln(command.OutOrStdout(), "Applied ENV affects new processes; running processes retain their existing environment.")
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
	personal := *client
	if err := personal.SetWorkspace("personal"); err != nil {
		return environmentVariableTarget{}, err
	}
	return environmentVariableTargetForCommand(command, &personal, requestedMachine)
}
