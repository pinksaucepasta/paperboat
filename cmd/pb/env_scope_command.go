package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
	"github.com/pinksaucepasta/paperboat/internal/environmentmanager"
	"github.com/spf13/cobra"
)

type vaultScopeTarget struct {
	kind    string
	owner   string
	machine string
	label   string
}

type vaultScopeMetadata struct {
	OwnerKind  string
	OwnerID    string
	MachineID  string
	KeyEpoch   uint64
	Revision   uint64
	DocumentID string
	Names      []string
}

func addVaultScopeCommands(root *cobra.Command) {
	rotate := &cobra.Command{
		Use:   "rotate",
		Short: "Rotate personal ENV scope keys while preserving values",
		Args:  commandArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			manager, err := passwordVaultForCommand(command)
			if err != nil {
				return err
			}
			if err := manager.RotatePersonal(command.Context()); err != nil {
				return safeEnvironmentVariableCommandError(err)
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), "Rotated personal encrypted ENV scope keys.")
			return err
		},
	}
	cancel := &cobra.Command{
		Use:   "cancel",
		Short: "Cancel an uncommitted personal ENV key rotation",
		Args:  commandArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			manager, err := passwordVaultForCommand(command)
			if err != nil {
				return err
			}
			confirmation, _ := command.Flags().GetString("confirm")
			expected := "CANCEL ENV ROTATE " + manager.AccountID
			if confirmation != expected {
				return invocationError(errors.New("rotation cancel requires --confirm `CANCEL ENV ROTATE <account_id>`"))
			}
			if err := manager.AbortPersonalRotation(command.Context()); err != nil {
				return safeEnvironmentVariableCommandError(err)
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), "Canceled the personal encrypted ENV key rotation.")
			return err
		},
	}
	cancel.Flags().String("confirm", "", "exact confirmation phrase: CANCEL ENV ROTATE <account_id>")
	rotate.AddCommand(cancel)
	root.AddCommand(rotate)

	team := &cobra.Command{Use: "team", Short: "Manage encrypted ENV team scopes", Args: commandArgs(cobra.NoArgs)}
	create := &cobra.Command{
		Use:   "create <team>",
		Short: "Create an encrypted ENV team scope",
		Args:  commandArgs(cobra.ExactArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {
			return runVaultTeamCreate(command, args[0])
		},
	}
	grant := &cobra.Command{
		Use:   "grant <team> <account>",
		Short: "Grant an account access to an encrypted ENV team scope",
		Args:  commandArgs(cobra.ExactArgs(2)),
		RunE: func(command *cobra.Command, args []string) error {
			return runVaultTeamGrant(command, args[0], args[1])
		},
	}
	teamRotate := &cobra.Command{
		Use:   "rotate <team>",
		Short: "Rotate an encrypted ENV team key while preserving its values",
		Args:  commandArgs(cobra.ExactArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {
			return runVaultTeamRotate(command, args[0])
		},
	}
	teamRotate.Flags().String("confirm", "", "exact confirmation phrase: ROTATE ENV TEAM <team>")
	revoke := &cobra.Command{
		Use:   "revoke <team>",
		Short: "Rotate an ENV team scope and revoke selected members",
		Args:  commandArgs(cobra.ExactArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {
			remove, _ := command.Flags().GetStringArray("remove")
			totalLoss, _ := command.Flags().GetBool("total-loss")
			confirm, _ := command.Flags().GetString("confirm")
			return runVaultTeamRevoke(command, args[0], remove, totalLoss, confirm)
		},
	}
	revoke.Flags().StringArray("remove", nil, "account to remove; repeat for multiple accounts")
	revoke.Flags().Bool("total-loss", false, "discard the previous team values while rotating the team key")
	revoke.Flags().String("confirm", "", "exact confirmation phrase for this destructive rotation")
	reset := &cobra.Command{
		Use:   "reset <team>",
		Short: "Discard all values and replace an encrypted ENV team key",
		Args:  commandArgs(cobra.ExactArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {
			teamID := args[0]
			confirmation, _ := command.Flags().GetString("confirm")
			if !vaultCommandIdentifier(teamID) || confirmation != "RESET ENV TEAM "+teamID {
				return invocationError(errors.New("team reset requires --confirm `RESET ENV TEAM <team>`"))
			}
			manager, err := passwordVaultForCommand(command)
			if err != nil {
				return err
			}
			if err := manager.RotateTeam(command.Context(), teamID, nil, true); err != nil {
				return safeEnvironmentVariableCommandError(err)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "Reset encrypted ENV team %s; previous values were discarded.\n", teamID)
			return err
		},
	}
	reset.Flags().String("confirm", "", "exact confirmation phrase: RESET ENV TEAM <team>")
	team.AddCommand(create, grant, teamRotate, revoke, reset)
	root.AddCommand(team)

	grants := &cobra.Command{Use: "grants", Short: "Reconcile encrypted ENV team grants", Args: commandArgs(cobra.NoArgs)}
	grants.AddCommand(&cobra.Command{
		Use:   "sync",
		Short: "Accept pending team grants into this account's encrypted vault",
		Args:  commandArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			manager, err := passwordVaultForCommand(command)
			if err != nil {
				return err
			}
			if err := manager.SyncTeamGrants(command.Context()); err != nil {
				return safeEnvironmentVariableCommandError(err)
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), "ENV team grants synchronized into encrypted vault custody.")
			return err
		},
	})
	root.AddCommand(grants)

	host := &cobra.Command{Use: "host", Short: "Manage encrypted host ENV projections", Args: commandArgs(cobra.NoArgs)}
	provision := &cobra.Command{
		Use:   "provision",
		Short: "Provision an explicit encrypted ENV selection to a host",
		Args:  commandArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			machine, _ := command.Flags().GetString("machine")
			selection, _ := command.Flags().GetStringArray("select")
			empty, _ := command.Flags().GetBool("empty")
			return runVaultHostProvision(command, machine, selection, empty)
		},
	}
	provision.Flags().String("machine", "", "machine name or ID")
	provision.Flags().StringArray("select", nil, "selection reference personal:NAME or team:TEAM:NAME; repeat to select more values")
	provision.Flags().Bool("empty", false, "explicitly provision an empty selection")
	host.AddCommand(provision)
	root.AddCommand(host)
}

func setEnvironmentVariableForScope(command *cobra.Command, team, requestedMachine, name string, valueStdin bool, valueFile string) error {
	if err := validateEnvironmentVariableNameForCLI(name); err != nil {
		return invocationError(err)
	}
	if err := validateVaultScopeFlags(team, requestedMachine); err != nil {
		return err
	}
	client, err := environmentVariableBackendForCommand(command)
	if err != nil {
		return err
	}
	manager, err := passwordVaultForCommand(command)
	if err != nil {
		return err
	}
	target, err := vaultScopeTargetForCommand(command, client, manager.AccountID, team, requestedMachine)
	if err != nil {
		return err
	}
	value, err := readEnvironmentVariableValueFile(command, valueStdin, valueFile)
	if err != nil {
		return err
	}
	defer clear(value)
	if err := manager.MutateScope(command.Context(), target.kind, target.owner, target.machine, func(values map[string][]byte) error {
		canonical, exists := vaultScopeConfiguredName(values, name)
		if exists {
			clear(values[canonical])
			values[canonical] = append([]byte(nil), value...)
			return nil
		}
		values[name] = append([]byte(nil), value...)
		return nil
	}); err != nil {
		return safeEnvironmentVariableCommandError(err)
	}
	_, err = fmt.Fprintf(command.OutOrStdout(), "Set %s in %s (encrypted vault scope).\n", name, target.label)
	return err
}

func unsetEnvironmentVariableForScope(command *cobra.Command, team, requestedMachine, name string, confirmed bool) error {
	if !confirmed {
		return invocationError(errors.New("environment variable removal requires --yes"))
	}
	if err := validateEnvironmentVariableNameForCLI(name); err != nil {
		return invocationError(err)
	}
	if err := validateVaultScopeFlags(team, requestedMachine); err != nil {
		return err
	}
	client, err := environmentVariableBackendForCommand(command)
	if err != nil {
		return err
	}
	manager, err := passwordVaultForCommand(command)
	if err != nil {
		return err
	}
	target, err := vaultScopeTargetForCommand(command, client, manager.AccountID, team, requestedMachine)
	if err != nil {
		return err
	}
	if err := manager.MutateScope(command.Context(), target.kind, target.owner, target.machine, func(values map[string][]byte) error {
		canonical, exists := vaultScopeConfiguredName(values, name)
		if !exists {
			return environmentmanager.ErrVariableNotConfigured
		}
		clear(values[canonical])
		delete(values, canonical)
		return nil
	}); err != nil {
		return safeEnvironmentVariableCommandError(err)
	}
	_, err = fmt.Fprintf(command.OutOrStdout(), "Unset %s from %s (encrypted vault scope).\n", name, target.label)
	return err
}

func vaultScopeConfiguredName(values map[string][]byte, requested string) (string, bool) {
	for name := range values {
		if strings.EqualFold(name, requested) {
			return name, true
		}
	}
	return "", false
}

func vaultScopeTargetForCommand(command *cobra.Command, client *api.Client, accountID, team, requestedMachine string) (vaultScopeTarget, error) {
	team = strings.TrimSpace(team)
	requestedMachine = strings.TrimSpace(requestedMachine)
	if err := validateVaultScopeFlags(team, requestedMachine); err != nil {
		return vaultScopeTarget{}, err
	}
	if team != "" {
		if !vaultCommandIdentifier(team) {
			return vaultScopeTarget{}, invocationError(errors.New("team must be a valid account identifier"))
		}
		return vaultScopeTarget{kind: "team", owner: team, label: "team " + team}, nil
	}
	target, err := environmentVariableTargetForCommand(command, client, requestedMachine)
	if err != nil {
		return vaultScopeTarget{}, err
	}
	label := "personal"
	if target.machineID != "" {
		label = "personal machine " + environmentVariableScopeLabel(target)
	}
	return vaultScopeTarget{kind: "personal", owner: accountID, machine: target.machineID, label: label}, nil
}

func validateVaultScopeFlags(team, requestedMachine string) error {
	if strings.TrimSpace(team) != "" && strings.TrimSpace(requestedMachine) != "" {
		return invocationError(errors.New("--team cannot be combined with --machine"))
	}
	return nil
}

func vaultCommandIdentifier(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for index, char := range value {
		if index == 0 && !((char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9')) {
			return false
		}
		if !((char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '_' || char == '-') {
			return false
		}
	}
	return true
}

func listEnvironmentVariablesForScope(command *cobra.Command, team, requestedMachine string, jsonOutput bool) error {
	if err := validateVaultScopeFlags(team, requestedMachine); err != nil {
		return err
	}
	client, err := environmentVariableBackendForCommand(command)
	if err != nil {
		return err
	}
	manager, err := passwordVaultForCommand(command)
	if err != nil {
		return err
	}
	target, err := vaultScopeTargetForCommand(command, client, manager.AccountID, team, requestedMachine)
	if err != nil {
		return err
	}
	metadata, err := readVaultScopeMetadata(command.Context(), manager, target)
	if err != nil {
		return safeEnvironmentVariableCommandError(err)
	}
	if jsonOutput {
		return writeVaultScopeMetadataJSON(command.OutOrStdout(), metadata)
	}
	return writeVaultScopeMetadataTable(command.OutOrStdout(), metadata, target.label)
}

func readVaultScopeMetadata(ctx context.Context, manager environmentmanager.PasswordVault, target vaultScopeTarget) (vaultScopeMetadata, error) {
	if ctx == nil {
		return vaultScopeMetadata{}, environmente2ee.ErrInvalid
	}
	data, ok := manager.Client.(environmentmanager.VaultDataClient)
	if !ok {
		return vaultScopeMetadata{}, environmente2ee.ErrInvalid
	}
	names, err := manager.ListScopeNames(ctx, target.kind, target.owner, target.machine)
	if err != nil {
		return vaultScopeMetadata{}, err
	}
	metadata := vaultScopeMetadata{OwnerKind: target.kind, OwnerID: target.owner, MachineID: target.machine, Names: names}
	state, err := data.GetVaultScope(ctx, target.kind, target.owner, target.machine)
	if api.IsNotFound(err) {
		return metadata, nil
	}
	if err != nil {
		return vaultScopeMetadata{}, err
	}
	scope, err := state.Decode()
	if err != nil || scope.Claims.Issuer != manager.Issuer || scope.Claims.OwnerKind != target.kind || scope.Claims.OwnerID != target.owner || scope.Claims.MachineID != target.machine {
		return vaultScopeMetadata{}, environmentmanager.ErrIntegrity
	}
	metadata.KeyEpoch = scope.Claims.KeyEpoch
	metadata.Revision = scope.Claims.Revision
	metadata.DocumentID = scope.ID.String()
	sort.Strings(metadata.Names)
	return metadata, nil
}

func writeVaultScopeMetadataTable(output io.Writer, metadata vaultScopeMetadata, label string) error {
	if _, err := fmt.Fprintf(output, "SCOPE\t%s\tKEY_EPOCH\t%d\tREVISION\t%d\n", label, metadata.KeyEpoch, metadata.Revision); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(output, "NAME\tCONFIGURED"); err != nil {
		return err
	}
	for _, name := range metadata.Names {
		if _, err := fmt.Fprintf(output, "%s\tyes\n", name); err != nil {
			return err
		}
	}
	return nil
}

func writeVaultScopeMetadataJSON(output io.Writer, metadata vaultScopeMetadata) error {
	return json.NewEncoder(output).Encode(map[string]any{
		"schema_version": "1.0",
		"ok":             true,
		"data": struct {
			OwnerKind  string   `json:"owner_kind"`
			OwnerID    string   `json:"owner_id"`
			MachineID  string   `json:"machine_id,omitempty"`
			KeyEpoch   uint64   `json:"key_epoch"`
			Revision   uint64   `json:"revision"`
			DocumentID string   `json:"document_id,omitempty"`
			Names      []string `json:"names"`
		}{metadata.OwnerKind, metadata.OwnerID, metadata.MachineID, metadata.KeyEpoch, metadata.Revision, metadata.DocumentID, metadata.Names},
	})
}

func runVaultTeamCreate(command *cobra.Command, teamID string) error {
	if !vaultCommandIdentifier(teamID) {
		return invocationError(errors.New("team must be a valid account identifier"))
	}
	manager, err := passwordVaultForCommand(command)
	if err != nil {
		return err
	}
	client, err := backendForCommand(command)
	if err != nil {
		return err
	}
	expectedGeneration := uint64(0)
	membershipGeneration := uint64(1)
	team, getErr := client.GetTeam(command.Context(), teamID)
	if getErr == nil {
		expectedGeneration = team.Generation
		membershipGeneration = 0
		for _, member := range team.Members {
			if member.AccountID == manager.AccountID && member.Active && member.Role == "owner" {
				membershipGeneration = member.MembershipGeneration
			}
		}
		if membershipGeneration == 0 {
			return errors.New("current account is not the accepted team owner")
		}
	} else if !api.IsNotFound(getErr) {
		return getErr
	}
	if err := manager.CreateTeamAt(command.Context(), teamID, expectedGeneration, membershipGeneration); err != nil {
		return safeEnvironmentVariableCommandError(err)
	}
	_, err = fmt.Fprintf(command.OutOrStdout(), "Created encrypted ENV team %s.\n", teamID)
	return err
}

func runVaultTeamGrant(command *cobra.Command, teamID, accountID string) error {
	if !vaultCommandIdentifier(teamID) || !vaultCommandIdentifier(accountID) {
		return invocationError(errors.New("team and account must be valid account identifiers"))
	}
	manager, err := passwordVaultForCommand(command)
	if err != nil {
		return err
	}
	if err := manager.GrantTeam(command.Context(), teamID, accountID); err != nil {
		return safeEnvironmentVariableCommandError(err)
	}
	_, err = fmt.Fprintf(command.OutOrStdout(), "Granted account %s access to encrypted ENV team %s.\n", accountID, teamID)
	return err
}

func runVaultTeamRotate(command *cobra.Command, teamID string) error {
	if !vaultCommandIdentifier(teamID) {
		return invocationError(errors.New("team must be a valid account identifier"))
	}
	confirmation, _ := command.Flags().GetString("confirm")
	if confirmation != "ROTATE ENV TEAM "+teamID {
		return invocationError(errors.New("team rotation requires --confirm `ROTATE ENV TEAM <team>`"))
	}
	manager, err := passwordVaultForCommand(command)
	if err != nil {
		return err
	}
	if err := manager.RotateTeam(command.Context(), teamID, nil, false); err != nil {
		return safeEnvironmentVariableCommandError(err)
	}
	_, err = fmt.Fprintf(command.OutOrStdout(), "Rotated encrypted ENV team %s while preserving values.\n", teamID)
	return err
}

func runVaultTeamRevoke(command *cobra.Command, teamID string, remove []string, totalLoss bool, confirmation string) error {
	if !vaultCommandIdentifier(teamID) {
		return invocationError(errors.New("team must be a valid account identifier"))
	}
	if totalLoss {
		if len(remove) != 0 || confirmation != "RESET ENV TEAM "+teamID {
			return invocationError(errors.New("--total-loss requires --confirm `RESET ENV TEAM <team>` and no --remove flags"))
		}
	} else {
		if confirmation != "ROTATE ENV TEAM "+teamID {
			return invocationError(errors.New("team revoke requires --confirm `ROTATE ENV TEAM <team>`"))
		}
	}
	seen := make(map[string]struct{}, len(remove))
	for _, accountID := range remove {
		if !vaultCommandIdentifier(accountID) {
			return invocationError(errors.New("removed accounts must be valid account identifiers"))
		}
		if _, ok := seen[accountID]; ok {
			return invocationError(errors.New("an account may be removed only once"))
		}
		seen[accountID] = struct{}{}
	}
	manager, err := passwordVaultForCommand(command)
	if err != nil {
		return err
	}
	if err := manager.RotateTeam(command.Context(), teamID, append([]string(nil), remove...), totalLoss); err != nil {
		return safeEnvironmentVariableCommandError(err)
	}
	_, err = fmt.Fprintf(command.OutOrStdout(), "Rotated encrypted ENV team %s.\n", teamID)
	return err
}

func runVaultHostProvision(command *cobra.Command, requestedMachine string, references []string, explicitEmpty bool) error {
	requestedMachine = strings.TrimSpace(requestedMachine)
	if requestedMachine == "" {
		return invocationError(errors.New("host provision requires --machine"))
	}
	if explicitEmpty && len(references) != 0 {
		return invocationError(errors.New("choose --empty or --select, not both"))
	}
	if len(references) == 0 && !explicitEmpty {
		return invocationError(errors.New("host provision requires at least one --select or explicit --empty"))
	}
	manager, err := passwordVaultForCommand(command)
	if err != nil {
		return err
	}
	selection, err := parseVaultHostSelections(references, manager.AccountID)
	if err != nil {
		return invocationError(err)
	}
	client, err := environmentVariableBackendForCommand(command)
	if err != nil {
		return err
	}
	target, err := environmentVariableTargetForCommand(command, client, requestedMachine)
	if err != nil {
		return err
	}
	if err := manager.ProvisionHost(command.Context(), target.machineID, selection); err != nil {
		return safeEnvironmentVariableCommandError(err)
	}
	_, err = fmt.Fprintf(command.OutOrStdout(), "Provisioned encrypted ENV selection for %s.\n", environmentVariableScopeLabel(target))
	return err
}

func parseVaultHostSelections(references []string, accountID string) ([]api.VaultHostSelection, error) {
	selection := make([]api.VaultHostSelection, 0, len(references))
	seen := make(map[string]struct{}, len(references))
	for _, reference := range references {
		parts := strings.Split(reference, ":")
		var item api.VaultHostSelection
		switch {
		case len(parts) == 2 && parts[0] == "personal":
			if err := validateEnvironmentVariableNameForCLI(parts[1]); err != nil {
				return nil, errors.New("personal selection name must be a valid environment variable name")
			}
			item = api.VaultHostSelection{OwnerKind: "personal", OwnerID: accountID, Name: parts[1]}
		case len(parts) == 3 && parts[0] == "team":
			if !vaultCommandIdentifier(parts[1]) {
				return nil, errors.New("team selection must use a valid team identifier")
			}
			if err := validateEnvironmentVariableNameForCLI(parts[2]); err != nil {
				return nil, errors.New("team selection name must be a valid environment variable name")
			}
			item = api.VaultHostSelection{OwnerKind: "team", OwnerID: parts[1], Name: parts[2]}
		default:
			return nil, errors.New("selection must be personal:NAME or team:TEAM:NAME")
		}
		key := item.OwnerKind + "\x00" + item.OwnerID + "\x00" + item.Name
		if _, ok := seen[key]; ok {
			return nil, errors.New("a host selection may appear only once")
		}
		seen[key] = struct{}{}
		selection = append(selection, item)
	}
	return selection, nil
}
