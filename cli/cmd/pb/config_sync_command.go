package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
	"github.com/spf13/cobra"
)

func configSyncCobraCommand() *cobra.Command {
	group := &cobra.Command{Use: "sync", Short: "Validate and apply this machine's configuration file"}
	path := &cobra.Command{Use: "path", Short: "Show the authoritative machine configuration path", Args: commandArgs(cobra.NoArgs), RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := configsync.DefaultMachineSourcePath()
		if err != nil {
			return err
		}
		if jsonOutputRequested(cmd) {
			return writeCLIJSON(cmd.OutOrStdout(), map[string]string{"path": path})
		}
		fmt.Fprintln(cmd.OutOrStdout(), path)
		return nil
	}}
	init := &cobra.Command{Use: "init", Short: "Create an empty machine configuration without overwriting existing files", Args: commandArgs(cobra.NoArgs), RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := configsync.DefaultMachineSourcePath()
		if err != nil {
			return err
		}
		if err := initMachineConfigSource(path); err != nil {
			return err
		}
		if jsonOutputRequested(cmd) {
			return writeCLIJSON(cmd.OutOrStdout(), map[string]string{"path": path})
		}
		fmt.Fprintln(cmd.OutOrStdout(), path)
		return nil
	}}
	path.Flags().Bool("json", false, "print the authoritative path as JSON")
	init.Flags().Bool("json", false, "print the initialized path as JSON")
	group.AddCommand(path, init)
	for _, operation := range []string{"validate", "apply"} {
		cmd := &cobra.Command{Use: operation, Short: "Review the effective file configuration on this machine", Args: commandArgs(cobra.NoArgs)}
		cmd.Flags().String("repository", "", "registered repository ID or name for initial bootstrap")
		cmd.Flags().Bool("json", false, "print safe effective projection as JSON")
		if operation == "apply" {
			cmd.Flags().String("confirm", "", "six-character confirmation code from the preview")
		}
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			return runConfigSyncSourceCommand(cmd, args, operation == "apply")
		}
		group.AddCommand(cmd)
	}
	return group
}

func initMachineConfigSource(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return configsync.ErrSourceConfigInvalid
	}
	if err := configsync.EnsureRepositoryCredentialRoot(filepath.Dir(path)); err != nil {
		return err
	}
	file, err := configsync.CreateSourceFile(path)
	if errors.Is(err, os.ErrExist) {
		return errors.New("machine configuration already exists; edit the existing file")
	}
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		file.Close()
		if !ok {
			os.Remove(path)
		}
	}()
	if _, err = file.WriteString("# Paperboat config sync\n"); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

type configSyncSourceBackend interface {
	ListConfigRepositories(context.Context) ([]api.ConfigRepository, error)
	ConfigRepositoryReadAccess(context.Context, string, string) (configsync.RepositoryReadAccess, error)
}
type configSyncProjection struct {
	SharedRevision     string                        `json:"shared_revision,omitempty"`
	Revision           string                        `json:"revision"`
	Enabled            bool                          `json:"enabled"`
	PullRepositoryID   string                        `json:"pull_repository_id"`
	PushRepositoryID   string                        `json:"push_repository_id"`
	Mode               string                        `json:"mode"`
	AutomaticUpdates   bool                          `json:"automatic_updates"`
	PathRules          []api.ConfigPathRule          `json:"path_rules"`
	RepositoryBindings []api.ConfigRepositoryBinding `json:"repository_bindings"`
}

func stringPointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
func assignmentBootstrapTarget(current api.ConfigAssignment) *configsync.RepositoryTarget {
	id := stringPointerValue(current.PullRepositoryID)
	if current.Mode == string(configsync.ModePushOnly) {
		id = ""
	}
	if id == "" && current.Mode != string(configsync.ModePullOnly) {
		id = stringPointerValue(current.PushRepositoryID)
	}
	if id == "" {
		return nil
	}
	target := &configsync.RepositoryTarget{RepositoryID: id}
	for _, binding := range current.RepositoryBindings {
		if binding.RepositoryID == id {
			target.URL = binding.URL
			break
		}
	}
	return target
}
func effectivePrimaryTarget(effective configsync.EffectiveConfig) *configsync.RepositoryTarget {
	if effective.Mode == configsync.ModePushOnly {
		return effective.Push
	}
	if effective.Mode == configsync.ModePullOnly {
		return effective.Pull
	}
	if effective.Pull != nil {
		return effective.Pull
	}
	return effective.Push
}
func sameSourceTarget(left, right *configsync.RepositoryTarget) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func buildConfigSyncProjection(ctx context.Context, backend configSyncSourceBackend, machine configsync.SourceConfig, requested string, current api.ConfigAssignment, cacheRoot, credentialRoot, home string) (configSyncProjection, error) {
	repos, err := backend.ListConfigRepositories(ctx)
	if err != nil {
		return configSyncProjection{}, friendlyCommandError(err)
	}
	mode := configsync.ModeBidirectional
	if machine.Mode != nil {
		mode = *machine.Mode
	}
	primary := effectivePrimaryTarget(configsync.EffectiveConfig{Mode: mode, Pull: machine.Pull, Push: machine.Push})
	bootstrap := assignmentBootstrapTarget(current)
	if requested != "" {
		repository, err := resolveConfigRepository(repos, requested)
		if err != nil {
			return configSyncProjection{}, err
		}
		bootstrap = &configsync.RepositoryTarget{RepositoryID: repository.ID}
	}
	if primary == nil {
		primary = bootstrap
	}
	shared := configsync.SourceConfig{}
	grants := map[string]configsync.RepositoryReadAccess{}
	resolve := func(target *configsync.RepositoryTarget) (api.ConfigRepository, configsync.RepositoryReadAccess, error) {
		repository, err := resolveConfigRepository(repos, target.RepositoryID)
		if err != nil {
			return api.ConfigRepository{}, configsync.RepositoryReadAccess{}, err
		}
		key := repository.ID + "\x00" + target.URL
		access, exists := grants[key]
		if !exists {
			access, err = backend.ConfigRepositoryReadAccess(ctx, repository.ID, target.URL)
			if err != nil {
				return api.ConfigRepository{}, configsync.RepositoryReadAccess{}, friendlyCommandError(err)
			}
			grants[key] = access
		}
		if access.RepositoryID != repository.ID || (target.URL != "" && access.CloneURL != target.URL) || (target.Branch != "" && target.Branch != access.Branch) {
			return api.ConfigRepository{}, configsync.RepositoryReadAccess{}, errors.New("repository grant does not match the source target or branch")
		}
		normalized, transport, err := configsync.NormalizeRepositoryEndpoint(access.CloneURL)
		if err != nil || normalized != access.CloneURL || transport != access.Transport || access.Capability != "repository_contents_read" || !access.ExpiresAt.After(time.Now()) {
			return api.ConfigRepository{}, configsync.RepositoryReadAccess{}, configsync.ErrAuthorization
		}
		return repository, access, nil
	}
	var effective configsync.EffectiveConfig
	var sharedRevision string
	for attempt := 0; attempt < 2; attempt++ {
		if primary != nil {
			_, access, err := resolve(primary)
			if err != nil {
				return configSyncProjection{}, err
			}
			shared, sharedRevision, err = configsync.ReadSharedRepositoryConfig(ctx, cacheRoot, credentialRoot, access, configsync.DefaultSourceConfigLimits())
			if err != nil {
				return configSyncProjection{}, err
			}
		}
		effective, err = configsync.MergeSourceConfigs(shared, machine)
		if err != nil {
			return configSyncProjection{}, err
		}
		selected := effectivePrimaryTarget(effective)
		if selected == nil {
			selected = bootstrap
		}
		if sameSourceTarget(primary, selected) {
			break
		}
		if attempt == 1 {
			return configSyncProjection{}, errors.New("shared configuration changes its primary repository repeatedly; define a stable machine target")
		}
		primary = selected
		shared = configsync.SourceConfig{}
	}
	if _, err := configsync.ResolveEffectiveConfig(home, effective); err != nil {
		return configSyncProjection{}, err
	}
	projection := configSyncProjection{Revision: effective.Revision, SharedRevision: sharedRevision, Enabled: effective.Enabled, Mode: string(effective.Mode), AutomaticUpdates: effective.AutomaticUpdates, PathRules: []api.ConfigPathRule{}, RepositoryBindings: []api.ConfigRepositoryBinding{}}
	for _, rule := range effective.PathRules {
		projection.PathRules = append(projection.PathRules, api.ConfigPathRule{ID: rule.ID, Source: rule.Source, RepositoryPath: rule.RepositoryPath, LocalPath: rule.LocalPath, Kind: rule.Kind, Include: rule.Include, Exclude: rule.Exclude})
	}
	if !effective.Enabled {
		return projection, nil
	}
	targets := []*configsync.RepositoryTarget{effective.Pull, effective.Push}
	for direction := range targets {
		if targets[direction] != nil {
			continue
		}
		targets[direction] = bootstrap
		if requested == "" {
			id := stringPointerValue(current.PullRepositoryID)
			if direction == 1 {
				id = stringPointerValue(current.PushRepositoryID)
			}
			if id != "" {
				targets[direction] = &configsync.RepositoryTarget{RepositoryID: id}
				for _, binding := range current.RepositoryBindings {
					if binding.RepositoryID == id {
						targets[direction].URL = binding.URL
						break
					}
				}
			}
		}
		if targets[direction] == nil {
			targets[direction] = primary
		}
	}
	for direction, target := range targets {
		if direction == 0 && effective.Mode == configsync.ModePushOnly || direction == 1 && effective.Mode == configsync.ModePullOnly {
			continue
		}
		if target == nil {
			return configSyncProjection{}, errors.New("choose --repository or define pull/push repository_id in the machine configuration")
		}
		repository, _, err := resolve(target)
		if err != nil {
			return configSyncProjection{}, err
		}
		if direction == 0 {
			projection.PullRepositoryID = repository.ID
		} else {
			projection.PushRepositoryID = repository.ID
		}
		if target.URL != "" {
			found := false
			for _, binding := range projection.RepositoryBindings {
				if binding.RepositoryID == repository.ID {
					if binding.URL != target.URL {
						return configSyncProjection{}, errors.New("one repository cannot have conflicting endpoint bindings")
					}
					found = true
				}
			}
			if !found {
				projection.RepositoryBindings = append(projection.RepositoryBindings, api.ConfigRepositoryBinding{RepositoryID: repository.ID, URL: target.URL})
			}
		}
	}
	return projection, nil
}

func runConfigSyncSourceCommand(cmd *cobra.Command, args []string, apply bool) error {
	return runConfigSyncSourceCommandWithService(cmd, args, apply, manageConfigService)
}
func runConfigSyncSourceCommandWithService(cmd *cobra.Command, args []string, apply bool, manage func(context.Context, string, bool) error) error {
	path, err := configsync.DefaultMachineSourcePath()
	if err != nil {
		return err
	}
	machine, err := configsync.LoadSourceConfig(path, configsync.DefaultSourceConfigLimits())
	if err != nil {
		return err
	}
	store, err := runtimeIdentityStore()
	if err != nil {
		return err
	}
	registration, err := store.Registration()
	if err != nil || registration.MachineID == "" {
		return errors.New("this machine is not enrolled; complete Paperboat setup before configuring sync")
	}
	client, err := backendClient(actionContext(cmd, args))
	if err != nil {
		return err
	}
	current, err := client.ConfigAssignment(cmd.Context(), registration.MachineID)
	if err != nil && !api.IsNotFound(err) {
		return friendlyCommandError(err)
	}
	credentialRoot, err := configRepositoryCredentialRoot()
	if err != nil {
		return err
	}
	stateRoot, err := previewRuntimeStateRoot()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	requested, _ := cmd.Flags().GetString("repository")
	projection, err := buildConfigSyncProjection(cmd.Context(), client, machine, requested, current, filepath.Join(stateRoot, "config-sync", "repository-read"), credentialRoot, home)
	if err != nil {
		return err
	}
	if apply {
		c := actionContext(cmd, args)
		c.Context = context.WithValue(c.Context, confirmationCommandKey{}, cmd)
		scope := configSyncProjectionConfirmationScope(registration.MachineID, current.Version, projection)
		impact := fmt.Sprintf("Apply configuration revision %s to this machine (%s), %s, %d explicit mapped paths? Synced files are ordinary plaintext in the Git repository. Repository collaborators may access synced content, and Git history retains prior content after removal. Disabled sync removes this machine's assignment and keeps repository content.", projection.Revision, registration.MachineID, projection.Mode, len(projection.PathRules))
		if err := confirmContextMutation(c, scope, impact); err != nil {
			return err
		}
		if !projection.Enabled {
			if current.ID != "" {
				if err := manage(cmd.Context(), registration.MachineID, false); err != nil {
					return err
				}
				if err := client.UnassignConfig(cmd.Context(), registration.MachineID, current.Version); err != nil {
					return errors.Join(friendlyCommandError(err), manage(cmd.Context(), registration.MachineID, true))
				}
			}
		} else {
			assignment, err := client.ConfigureConfigProjection(cmd.Context(), registration.MachineID, projection.PullRepositoryID, projection.PushRepositoryID, projection.Mode, projection.AutomaticUpdates, current.Version, projection.PathRules, projection.RepositoryBindings, projection.Revision)
			if err != nil {
				return friendlyCommandError(err)
			}
			if assignment.ConsentState == "pending" {
				warning, err := client.ConfigWarning(cmd.Context(), registration.MachineID)
				if err != nil {
					return friendlyCommandError(err)
				}
				if warning.Revision == "" || warning.RepositoryVisibility == "" || warning.HistoryRetention == "" || warning.AccessConsequence == "" {
					return errors.New("server returned an incomplete configuration consent warning")
				}
				if _, err := client.AcceptConfigConsent(cmd.Context(), registration.MachineID, warning.Revision, assignment.Version); err != nil {
					return friendlyCommandError(err)
				}
			}
			if err := manage(cmd.Context(), registration.MachineID, true); err != nil {
				return err
			}
		}
	}
	jsonOutput, _ := cmd.Flags().GetBool("json")
	if jsonOutput {
		return writeCLIJSON(cmd.OutOrStdout(), projection)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Machine configuration: %s\nRevision: %s\nEnabled: %t · %s · %d path rules\n", path, projection.Revision, projection.Enabled, projection.Mode, len(projection.PathRules))
	if apply {
		fmt.Fprintln(cmd.OutOrStdout(), "Configuration applied to this machine.")
	}
	return nil
}

func configSyncProjectionConfirmationScope(machineID string, version int64, projection configSyncProjection) string {
	data, _ := json.Marshal(projection)
	digest := sha256.Sum256(data)
	return fmt.Sprintf("config-sync-source:%s:%d:%x", machineID, version, digest)
}
