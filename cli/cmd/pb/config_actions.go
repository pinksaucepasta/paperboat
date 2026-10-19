package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/endpointbinary"
	helperconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	service "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/spf13/cobra"
)

// configuredSSHTargetPort is the SSH target port `pb config set ssh-target-port` stored in this
// machine's registration. It reports false on a machine that was never enrolled; it never creates
// any state.
func configuredSSHTargetPort() (uint16, bool) {
	stateRoot := os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT")
	if stateRoot == "" {
		var err error
		if stateRoot, err = helperconfig.DefaultStateRoot(os.Getenv); err != nil {
			return 0, false
		}
	}
	if info, err := os.Stat(stateRoot); err != nil || !info.IsDir() {
		return 0, false
	}
	store, err := identity.Open(identity.Config{StateRoot: stateRoot})
	if err != nil {
		return 0, false
	}
	registration, err := store.Registration()
	if err != nil || registration.SSHPort == 0 {
		return 0, false
	}
	return registration.SSHPort, true
}

func configSetSSHTargetPort(c *command.Context, port uint16) error {
	stateRoot := os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT")
	var err error
	if stateRoot == "" {
		stateRoot, err = helperconfig.DefaultStateRoot(os.Getenv)
		if err != nil {
			return err
		}
	}
	store, err := identity.Open(identity.Config{StateRoot: stateRoot})
	if err != nil {
		return err
	}
	registration, err := store.Registration()
	if err != nil || registration.InstallationGeneration < 1 {
		return commandPreparationFailure{step: prepareMachineRegistration, cause: err}
	}
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	generation := uint64(registration.InstallationGeneration)
	current, err := client.ManagedSSHTarget(c.Context, registration.MachineID, generation)
	if err != nil {
		return err
	}
	if current.Port != port {
		if _, err := client.UpdateManagedSSHTargetPort(c.Context, registration.MachineID, generation, current.ReconciliationVersion, port, newIdempotencyKey()); err != nil {
			return err
		}
	}
	registration.SSHPort = port
	registration.UpdatedAt = time.Now().UTC()
	if err := store.SaveRegistration(registration); err != nil {
		return err
	}
	if c.Bool("json") {
		return writeCLIJSON(c.Writer, map[string]any{"key": "ssh-target-port", "port": port, "updated": true})
	}
	fmt.Fprintf(c.Writer, "SSH target port set to %d\n", port)
	return nil
}

func configUnassign(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	target, err := resolveEnvironmentTarget(c.Context, client, c.Args().First())
	if err != nil {
		return err
	}
	machineID := target.id
	assignment, err := client.ConfigAssignment(c.Context, machineID)
	if err != nil {
		return friendlyCommandError(err)
	}
	if err := confirmContextMutation(c, fmt.Sprintf("config-unassign:%s:%d", target.id, assignment.Version), fmt.Sprintf("Remove the config repository assignment from %s (%s)? The repository and its content will remain.", target.name, target.id)); err != nil {
		return err
	}
	if err := manageConfigService(c.Context, machineID, false); err != nil {
		return fmt.Errorf("stop config sync service: %w", err)
	}
	if err := client.UnassignConfig(c.Context, machineID, assignment.Version); err != nil {
		repairErr := manageConfigService(c.Context, machineID, true)
		return errors.Join(friendlyCommandError(err), repairErr)
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "environment": map[string]string{"id": target.id, "kind": target.kind, "alias": target.name}, "state": "unassigned", "outcome": "confirmed"})
	}
	fmt.Fprintf(c.Writer, "Removed config assignment from %s.\n", target.name)
	return nil
}

func configApproveRevision(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	target, err := resolveEnvironmentTarget(c.Context, client, c.Args().First())
	if err != nil {
		return err
	}
	status, err := client.ConfigSyncStatus(c.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	items, err := selectConfigEnvironments(status.Environments, target.id)
	if err != nil || len(items) != 1 {
		return errors.Join(errors.New("configuration revision is unavailable"), err)
	}
	item := items[0]
	if item.RemoteRevision == "" || item.AssignmentVersion < 1 {
		return errors.New("no reviewed configuration revision is pending")
	}
	if len(item.Review) > 0 && !c.Bool("json") {
		fmt.Fprintf(c.Writer, "Revision %s changes:\n", item.RemoteRevision)
		for _, change := range item.Review {
			fmt.Fprintf(c.Writer, "  %s  %s\n", change.Reason, change.Path)
		}
	}
	approved, err := client.ApproveConfigPullRevision(c.Context, target.id, item.RemoteRevision, item.AssignmentVersion)
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return writeCLIJSON(c.Writer, map[string]any{"environment": map[string]string{"id": target.id, "kind": target.kind, "alias": target.name}, "revision": item.RemoteRevision, "assignment": approved})
	}
	fmt.Fprintf(c.Writer, "Approved configuration revision %s for %s (assignment version %d).\n", item.RemoteRevision, target.name, approved.Version)
	return nil
}

func configTeamDefaultSet(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	repositories, err := client.ListConfigRepositories(c.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	repository, err := resolveConfigRepository(repositories, c.Args().Get(1))
	if err != nil {
		return err
	}
	version := int64(0)
	if current, getErr := client.ConfigTeamDefault(c.Context, c.Args().First()); getErr == nil {
		version = current.Version
	} else if !api.IsNotFound(getErr) {
		return friendlyCommandError(getErr)
	}
	item, err := client.SetConfigTeamDefault(c.Context, c.Args().First(), repository.ID, version)
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return writeCLIJSON(c.Writer, item)
	}
	fmt.Fprintf(c.Writer, "Team %s default pull repository is %s (version %d).\n", item.TeamID, item.DisplayName, item.Version)
	return nil
}

func configTeamDefaultAdopt(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	item, err := client.ConfigTeamDefault(c.Context, c.Args().First())
	if err != nil {
		return friendlyCommandError(err)
	}
	adoption, err := client.AdoptConfigTeamDefault(c.Context, item.TeamID, item.Version)
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return writeCLIJSON(c.Writer, adoption)
	}
	fmt.Fprintf(c.Writer, "Adopted %s from team %s at version %d; personal machine assignments still take precedence.\n", adoption.DisplayName, adoption.TeamID, adoption.AdoptedVersion)
	return nil
}

func configTeamDefaultUnadopt(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	if err := client.UnadoptConfigTeamDefault(c.Context); err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return writeCLIJSON(c.Writer, map[string]any{"adopted": false})
	}
	fmt.Fprintln(c.Writer, "Stopped inheriting the team configuration default; personal assignments were unchanged.")
	return nil
}

func manageConfigService(ctx context.Context, machineID string, install bool) error {
	stateRoot := strings.TrimSpace(os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT"))
	var err error
	if stateRoot == "" {
		stateRoot, err = helperconfig.DefaultStateRoot(os.Getenv)
		if err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "machine-registration.json")); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	identityStore, err := identity.Open(identity.Config{StateRoot: stateRoot})
	if err != nil {
		return err
	}
	registration, err := identityStore.Registration()
	if err != nil {
		return err
	}
	if registration.MachineID != machineID {
		return nil
	}
	if handled, windowsErr := manageWindowsConfigService(ctx, stateRoot, install); handled {
		return windowsErr
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if install {
		executable, err = endpointbinary.Daemon(executable)
	} else {
		executable, err = endpointbinary.DaemonPathForRemoval(executable)
	}
	if err != nil {
		return err
	}
	account, err := user.Current()
	if err != nil {
		return err
	}
	group, err := user.LookupGroupId(account.Gid)
	if err != nil {
		return err
	}
	query := &exec.Cmd{}
	prepareConfigServiceQuery(query)
	runner := service.ExecRunner{Environment: query.Env}
	var controller service.Controller
	switch runtime.GOOS {
	case "darwin":
		uid, parseErr := strconv.Atoi(account.Uid)
		if parseErr != nil {
			return parseErr
		}
		controller = service.LaunchdController{Runner: runner, UID: uid, Label: service.ConfigLabel, UserDomain: true}
	case "linux":
		controller = service.SystemdController{Runner: runner, Unit: "paperboat-runtime-config.service", User: true}
	default:
		return service.ErrUnsupportedPlatform
	}
	definition := service.Config{Platform: runtime.GOOS, Kind: service.ConfigKind, ConfigRoot: home, Executable: executable, User: account.Username, Group: group.Name, Arguments: []string{"daemon", "__runtime-config", "--state-root", stateRoot}, Environment: map[string]string{"HOME": home}, Controller: controller}
	if !install {
		return service.Remove(ctx, definition)
	}
	installer, err := service.New(definition)
	if err != nil {
		return err
	}
	return installer.Install(ctx)
}

func configStatus(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	status, err := client.ConfigSyncStatus(c.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	items, err := selectConfigEnvironments(status.Environments, c.Args().First())
	if err != nil {
		return err
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "state": status.State, "environments": items})
	}
	if len(items) == 0 {
		fmt.Fprintln(c.Writer, "No configuration assignments are reporting status.")
		return nil
	}
	for _, item := range items {
		name := item.Alias
		if name == "" {
			name = item.EnvironmentID
		}
		fmt.Fprintf(c.Writer, "%s: %s, %s, manifest %s, %d managed, %d pending clean\n",
			name, item.State, strings.ReplaceAll(item.Mode, "_", "-"), item.ManifestHealth,
			item.ManagedPathCount, item.PendingCleanPathCount)
		switch item.ErrorCode {
		case "configuration_changed":
			fmt.Fprintln(c.Writer, "  Configuration changed; run pb config sync apply on this machine to approve it.")
		case "configuration_invalid":
			fmt.Fprintln(c.Writer, "  Fix this machine's TOML file, then run pb config sync validate and pb config sync apply.")
		}
	}
	return nil
}

func configConflictCobraCommand() *cobra.Command {
	root := &cobra.Command{Use: "conflict", Short: "Inspect and resolve configuration conflicts"}
	list := &cobra.Command{Use: "list [environment]", Short: "List current path conflicts", Args: commandArgs(cobra.MaximumNArgs(1)), RunE: actionRun(configConflictList)}
	list.Flags().Bool("json", false, "print JSON")
	show := &cobra.Command{Use: "show <environment> <path>", Short: "Show a current path conflict", Args: commandArgs(cobra.ExactArgs(2)), RunE: actionRun(configConflictShow)}
	show.Flags().Bool("json", false, "print JSON")
	resolve := &cobra.Command{Use: "resolve <environment> <path>", Short: "Choose the machine or repository version", Args: commandArgs(cobra.ExactArgs(2)), RunE: actionRun(configConflictResolve)}
	resolve.Flags().String("keep", "", "version to keep: machine or repository")
	resolve.Flags().Bool("json", false, "print JSON")
	compare := &cobra.Command{Use: "compare <environment> <path>", Short: "Read current machine and repository conflict contents through an encrypted connection", Args: commandArgs(cobra.ExactArgs(2)), RunE: actionRun(configConflictCompare)}
	compare.Flags().Bool("json", false, "print comparison content as base64 JSON")
	root.AddCommand(list, show, compare, resolve)
	return root
}

func configForceCobraCommand() *cobra.Command {
	command := &cobra.Command{
		Use: "force <pull|push> <environment> [path]", Short: "Force a scoped configuration direction",
		Args: commandArgs(cobra.RangeArgs(2, 3)), RunE: actionRun(configForce),
	}
	command.Flags().String("confirm", "", "six-character confirmation code from the preview")
	command.Flags().Bool("json", false, "print JSON")
	return command
}

func configConflictList(c *command.Context) error {
	status, items, err := loadSelectedConfigStatus(c, c.Args().First())
	if err != nil {
		return err
	}
	type listedConflict struct {
		EnvironmentID   string                    `json:"environment_id"`
		EnvironmentName string                    `json:"environment_name"`
		Conflict        api.ConfigSyncPathSummary `json:"conflict"`
	}
	conflicts := make([]listedConflict, 0)
	for _, item := range items {
		for _, conflict := range item.Conflicts {
			conflicts = append(conflicts, listedConflict{item.EnvironmentID, item.Alias, conflict})
		}
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "state": status.State, "conflicts": conflicts})
	}
	if len(conflicts) == 0 {
		fmt.Fprintln(c.Writer, "No configuration conflicts.")
		return nil
	}
	for _, item := range conflicts {
		name := item.EnvironmentName
		if name == "" {
			name = item.EnvironmentID
		}
		fmt.Fprintf(c.Writer, "%s\t%s\t%s\n", name, item.Conflict.Path, strings.ReplaceAll(item.Conflict.Reason, "_", " "))
	}
	return nil
}

func configConflictShow(c *command.Context) error {
	_, items, err := loadSelectedConfigStatus(c, c.Args().First())
	if err != nil {
		return err
	}
	conflict, err := findConfigConflict(items[0], c.Args().Get(1))
	if err != nil {
		return err
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "environment": items[0], "conflict": conflict})
	}
	fmt.Fprintf(c.Writer, "%s on %s\nReason: %s\nChoices: keep this machine's version or use repository version.\n",
		conflict.Path, items[0].Alias, strings.ReplaceAll(conflict.Reason, "_", " "))
	return nil
}

func configConflictResolve(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	status, err := client.ConfigSyncStatus(c.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	items, err := selectConfigEnvironments(status.Environments, c.Args().First())
	if err != nil {
		return err
	}
	conflict, err := findConfigConflict(items[0], c.Args().Get(1))
	if err != nil {
		return err
	}
	action := ""
	switch strings.ToLower(strings.TrimSpace(c.String("keep"))) {
	case "machine", "local":
		action = "keep_local"
	case "repository", "remote":
		action = "keep_remote"
	default:
		return localArgumentError("config conflict resolve --keep must be machine or repository")
	}
	operation, err := client.ResolveConfigConflict(c.Context, items[0].EnvironmentID, api.ConfigConflictRequest{
		Path: conflict.Path, ConflictRevision: conflict.Revision, ExpectedRemoteRevision: items[0].RemoteRevision,
		ExpectedAssignmentVersion: items[0].AssignmentVersion, Action: action,
	})
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "operation": operation, "outcome": "queued"})
	}
	fmt.Fprintf(c.Writer, "Queued %s for %s on %s.\n", strings.ReplaceAll(action, "_", " "), conflict.Path, items[0].Alias)
	return nil
}

func configForce(c *command.Context) error {
	direction := strings.ToLower(strings.TrimSpace(c.Args().First()))
	if direction != "pull" && direction != "push" {
		return localArgumentError("config force direction must be pull or push")
	}
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	status, err := client.ConfigSyncStatus(c.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	items, err := selectConfigEnvironments(status.Environments, c.Args().Get(1))
	if err != nil {
		return err
	}
	item := items[0]
	request := api.ConfigForceRequest{
		Scope: "config", ExpectedRemoteRevision: item.RemoteRevision, ExpectedAssignmentVersion: item.AssignmentVersion,
		Action: "force_" + direction, Confirmation: "FORCE " + strings.ToUpper(direction),
	}
	if c.Args().Len() == 3 {
		conflict, findErr := findConfigConflict(item, c.Args().Get(2))
		if findErr != nil {
			return findErr
		}
		request.Scope, request.Path, request.ConflictRevision = "path", conflict.Path, conflict.Revision
	}
	if err := confirmContextMutation(c, "config-force:"+item.EnvironmentID+":"+direction+":"+request.Scope+":"+request.Path+":"+request.ConflictRevision, fmt.Sprintf("Force %s for %s scope on %s (%s)? This queues a recoverable config sync operation.", direction, request.Scope, item.Alias, item.EnvironmentID)); err != nil {
		return err
	}
	operation, err := client.ForceConfig(c.Context, item.EnvironmentID, request)
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "operation": operation, "outcome": "queued"})
	}
	fmt.Fprintf(c.Writer, "Queued force %s for %s on %s.\n", direction, request.Scope, item.Alias)
	return nil
}

func loadSelectedConfigStatus(c *command.Context, requested string) (api.ConfigSyncStatus, []api.ConfigSyncEnvironmentState, error) {
	client, err := backendClient(c)
	if err != nil {
		return api.ConfigSyncStatus{}, nil, err
	}
	status, err := client.ConfigSyncStatus(c.Context)
	if err != nil {
		return api.ConfigSyncStatus{}, nil, friendlyCommandError(err)
	}
	items, err := selectConfigEnvironments(status.Environments, requested)
	return status, items, err
}

func selectConfigEnvironments(items []api.ConfigSyncEnvironmentState, requested string) ([]api.ConfigSyncEnvironmentState, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return items, nil
	}
	matches := make([]api.ConfigSyncEnvironmentState, 0, 1)
	for _, item := range items {
		if item.EnvironmentID == requested || item.MachineID == requested || strings.EqualFold(item.Alias, requested) {
			matches = append(matches, item)
		}
	}
	if len(matches) == 1 {
		return matches, nil
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("configuration environment %q is ambiguous; use its stable ID", requested)
	}
	return nil, fmt.Errorf("configuration environment %q was not found", requested)
}

func findConfigConflict(item api.ConfigSyncEnvironmentState, requested string) (api.ConfigSyncPathSummary, error) {
	for _, conflict := range item.Conflicts {
		if conflict.Path == requested {
			return conflict, nil
		}
	}
	return api.ConfigSyncPathSummary{}, fmt.Errorf("configuration conflict %q was not found on %s", requested, item.Alias)
}

func resolveConfigRepository(items []api.ConfigRepository, requested string) (api.ConfigRepository, error) {
	for _, item := range items {
		if item.ID == requested {
			return item, nil
		}
	}
	matches := make([]api.ConfigRepository, 0, 1)
	for _, item := range items {
		if strings.EqualFold(item.DisplayName, requested) || strings.EqualFold(item.ExternalRef, requested) {
			matches = append(matches, item)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return api.ConfigRepository{}, fmt.Errorf("config repository %q is ambiguous; use a stable repository ID", requested)
	}
	return api.ConfigRepository{}, fmt.Errorf("config repository %q was not found", requested)
}
