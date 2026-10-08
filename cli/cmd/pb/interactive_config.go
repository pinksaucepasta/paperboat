package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
	"github.com/pinksaucepasta/paperboat/internal/prompt"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/spf13/cobra"
)

// Only guided config-sync actions use a terminal confirmation instead of the
// copy-and-repeat confirmation contract used by noninteractive commands.
type configSyncInteractiveConfirmation struct{}

type configSyncRegistrationFailure struct{ cause error }

func (configSyncRegistrationFailure) Error() string {
	return "Paperboat could not inspect this machine's enrollment for config sync."
}

func (e configSyncRegistrationFailure) Unwrap() error { return e.cause }

type configSyncSourceInspectionFailure struct{ cause error }

func (configSyncSourceInspectionFailure) Error() string {
	return "Paperboat could not inspect the machine's config sync file."
}

func (e configSyncSourceInspectionFailure) Unwrap() error { return e.cause }

func (configSyncSourceInspectionFailure) DiagnosticStage() string { return "reconciliation" }

func (configSyncSourceInspectionFailure) DiagnosticCode() string { return "config_sync_failed" }

type githubLinkCancelFailure struct{ cause error }

func (githubLinkCancelFailure) Error() string {
	return "The pending GitHub connection could not be canceled and will expire automatically."
}

func (e githubLinkCancelFailure) Unwrap() error { return e.cause }

// onlyNotExistCause accepts a missing-file outcome only when every bounded
// leaf is a not-exist cause. A joined operational error must remain visible.
func onlyNotExistCause(err error) bool {
	if err == nil {
		return false
	}
	pending := []error{err}
	leaves := 0
	steps := 0
	for len(pending) != 0 {
		if steps == 16 {
			return false
		}
		current := pending[0]
		pending = pending[1:]
		steps++
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return false
			}
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 || len(pending)+len(children) > 16-steps {
				return false
			}
			pending = append(pending, children...)
			continue
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			child := wrapped.Unwrap()
			if child == nil || len(pending)+1 > 16-steps {
				return false
			}
			pending = append(pending, child)
			continue
		}
		if !errors.Is(current, os.ErrNotExist) {
			return false
		}
		leaves++
	}
	return leaves != 0
}

func configSyncSourceExists(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return true, nil
	} else if onlyNotExistCause(err) {
		return false, nil
	} else {
		return false, configSyncSourceInspectionFailure{cause: err}
	}
}

func configSyncEnrollmentError(machineID string, err error) error {
	if err != nil {
		if onlyNotExistCause(err) {
			return errors.New("this machine is not enrolled; complete Paperboat setup before enabling config sync")
		}
		return configSyncRegistrationFailure{cause: err}
	}
	if machineID == "" {
		return errors.New("this machine is not enrolled; complete Paperboat setup before enabling config sync")
	}
	return nil
}

func githubLinkCleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
}

func joinGitHubLinkCancelFailure(parentErr, cancelErr error) error {
	if cancelErr == nil {
		return parentErr
	}
	return errors.Join(parentErr, githubLinkCancelFailure{cause: cancelErr})
}

func actionHomeConfigSync(command *cobra.Command) error {
	store, err := runtimeIdentityStore()
	if err != nil {
		return configSyncRegistrationFailure{cause: err}
	}
	registration, err := store.Registration()
	if err := configSyncEnrollmentError(registration.MachineID, err); err != nil {
		return err
	}
	client, err := backendForCommand(command)
	if err != nil {
		return err
	}
	machineID := registration.MachineID
	for {
		var assignment api.ConfigAssignment
		err := homeLoading(command, "Config sync", "Checking this machine's assignment", func(ctx context.Context) error {
			var loadErr error
			assignment, loadErr = client.ConfigAssignment(ctx, machineID)
			return loadErr
		})
		if err != nil {
			return friendlyCommandError(err)
		}
		choice, err := chooseHomeAction(command, "Config sync · this machine", configSyncMenuItems(assignment))
		if err != nil {
			return err
		}
		switch choice.ID {
		case "status":
			err = runHomeResult(command, []string{"config", "status", machineID})
		case "conflicts":
			err = reviewConfigConflictsInteractive(command, client, machineID)
		case "configure":
			err = configureSyncInteractive(command, client, machineID, assignment)
		case "disable":
			err = runConfigSyncResult(command, []string{"config", "unassign", machineID})
		case "credentials":
			repository, chooseErr := chooseConfigRepositoryInteractive(command, client, "Repository credentials", nil)
			err = chooseErr
			if err == nil {
				for _, binding := range assignment.RepositoryBindings {
					if binding.RepositoryID == repository.ID {
						repository.ExternalRef = binding.URL
						break
					}
				}
				err = configureRepositoryCredentialsInteractive(command, repository)
			}
		case "custom-repository":
			_, err = connectCustomConfigRepositoryInteractive(command, client)
		case "repositories":
			_, err = connectConfigRepositoryInteractive(command, client)
		case "github":
			err = connectGitHubInteractive(command, client)
		case "review":
			err = reviewConfigSyncInteractive(command, client, machineID)
		case "repair":
			if assignment.ConsentState == "pending" {
				err = configureSyncInteractive(command, client, machineID, assignment)
			} else {
				err = manageConfigService(command.Context(), machineID, true)
				if err == nil {
					err = showHomeText(command, "Config sync", "The configuration worker has been started. Open Status to check synchronization progress.")
				}
			}
		}
		if errors.Is(err, selector.ErrInterrupted) {
			return err
		}
		if err != nil && !interactiveCanceled(err) {
			if err = showHomeFailure(command, err); err != nil && !interactiveCanceled(err) {
				return err
			}
		}
	}
}

func configSyncMenuItems(assignment api.ConfigAssignment) []selector.Item {
	items := []selector.Item{{ID: "status", Title: "Status", Description: "Worker state, managed paths, pending changes and conflicts"}}
	if configAssignmentEnabled(assignment) {
		items = append(items,
			selector.Item{ID: "configure", Title: "Configure sync", Description: strings.ReplaceAll(assignment.Mode, "_", " ") + " · automatic updates " + onOff(assignment.AutomaticUpdates)},
			selector.Item{ID: "conflicts", Title: "Compare and resolve conflicts", Description: "Read this machine and repository versions through an encrypted connection"},
			selector.Item{ID: "review", Title: "Review pending updates", Description: "Inspect changes before approving a repository revision"},
			selector.Item{ID: "repair", Title: "Start or repair worker", Description: "Retry local worker installation without changing repositories"},
			selector.Item{ID: "disable", Title: "Disable sync", Description: "Stop this machine's worker and remove its assignment; keep repository content", Action: true})
	} else {
		items = append(items, selector.Item{ID: "configure", Title: "Enable sync", Description: "Choose repositories, sync direction and update behavior", Action: true})
	}
	return append(items, selector.Item{ID: "credentials", Title: "Repository credentials", Description: "Private machine-local HTTPS credentials or SSH key/agent and trusted hosts"}, selector.Item{ID: "custom-repository", Title: "Add custom Git repository", Description: "HTTPS, SSH, or an explicit local repository; GitHub is optional"}, selector.Item{ID: "repositories", Title: "Add repository", Description: "Choose a repository from your connected provider account"}, selector.Item{ID: "github", Title: "Connect GitHub", Description: "Approve account access in your browser, then return here"})
}

func runConfigSyncResult(parent *cobra.Command, args []string) error {
	original := parent.Context()
	parent.SetContext(context.WithValue(original, configSyncInteractiveConfirmation{}, true))
	defer parent.SetContext(original)
	return runHomeResult(parent, args)
}

func configureSyncInteractive(command *cobra.Command, client *api.Client, machineID string, current api.ConfigAssignment) error {
	path, err := configsync.DefaultMachineSourcePath()
	if err != nil {
		return err
	}
	exists, err := configSyncSourceExists(path)
	if err != nil {
		return err
	}
	if !exists {
		if err := runHomeResult(command, []string{"config", "sync", "init"}); err != nil {
			return err
		}
	}
	choice, err := chooseHomeAction(command, "File-based config sync", []selector.Item{
		{ID: "instructions", Title: "Configuration file", Description: path},
		{ID: "apply", Title: "Apply current configuration", Description: "Review file-defined destinations and approve sync", Action: true},
	})
	if err != nil {
		return err
	}
	if choice.ID == "instructions" {
		return showHomeText(command, "Config sync files", "Edit "+path+" for machine overrides. Optional repository defaults belong in .paperboat/config-sync.json. Shared defaults may include explicit OS destinations. An empty machine file inherits applicable defaults. Run pb config sync validate to review, then pb config sync apply to approve.")
	}
	repository, err := chooseConfigRepositoryInteractive(command, client, "Configuration repository", current.RepositoryID)
	if err != nil {
		return err
	}
	return runConfigSyncResult(command, []string{"config", "sync", "apply", "--repository", repository.ID})
}

func chooseConfigRepositoryInteractive(command *cobra.Command, client *api.Client, title string, current *string) (api.ConfigRepository, error) {
	for {
		var repositories []api.ConfigRepository
		err := homeLoading(command, title, "Loading connected repositories", func(ctx context.Context) error {
			var loadErr error
			repositories, loadErr = client.ListConfigRepositories(ctx)
			return loadErr
		})
		if err != nil {
			return api.ConfigRepository{}, friendlyCommandError(err)
		}
		items := make([]selector.Item, 0, len(repositories)+1)
		for _, repository := range repositories {
			if repository.State != "" && repository.State != "active" {
				continue
			}
			item := selector.Item{ID: repository.ID, Title: firstNonEmpty(repository.DisplayName, repository.ExternalRef), Description: repository.Provider + " · " + repository.ExternalRef}
			if current != nil && repository.ID == *current {
				items = append([]selector.Item{item}, items...)
			} else {
				items = append(items, item)
			}
		}
		items = append(items, selector.Item{ID: "custom", Title: "Add custom Git repository", Description: "HTTPS, SSH or local repository", Action: true}, selector.Item{ID: "add", Title: "Add repository", Description: "Choose from your connected provider account", Action: true})
		choice, err := chooseHomeAction(command, title, items)
		if err != nil {
			return api.ConfigRepository{}, err
		}
		if choice.ID == "custom" {
			return connectCustomConfigRepositoryInteractive(command, client)
		}
		if choice.ID == "add" {
			connected, err := connectConfigRepositoryInteractive(command, client)
			if err != nil {
				return api.ConfigRepository{}, err
			}
			return connected, nil
		}
		for _, repository := range repositories {
			if repository.ID == choice.ID {
				return repository, nil
			}
		}
	}
}

func connectConfigRepositoryInteractive(command *cobra.Command, client *api.Client) (api.ConfigRepository, error) {
	var candidates []api.ConfigRepositoryCandidate
	var err error
	for {
		err = homeLoading(command, "Add configuration repository", "Loading provider repositories", func(ctx context.Context) error {
			var loadErr error
			candidates, loadErr = client.ConfigRepositoryCandidates(ctx)
			return loadErr
		})
		if err == nil {
			break
		}
		{
			var apiErr *api.APIError
			if !errors.As(err, &apiErr) || apiErr.Code != "repository_candidates_unavailable" {
				return api.ConfigRepository{}, friendlyCommandError(err)
			}
			choice, chooseErr := chooseHomeAction(command, "Provider repositories unavailable", []selector.Item{
				{ID: "github", Title: "Connect GitHub", Description: "Authorize your account in the browser, then retry repository discovery"},
				{ID: "retry", Title: "Retry discovery", Description: "Use this after repairing an existing provider connection"},
			})
			if chooseErr != nil {
				return api.ConfigRepository{}, chooseErr
			}
			if choice.ID == "github" {
				if err = connectGitHubInteractive(command, client); err != nil {
					return api.ConfigRepository{}, err
				}
			}
		}
	}
	if len(candidates) == 0 {
		return api.ConfigRepository{}, errors.New("your connected provider exposes no configuration repositories; create or grant access to a private repository, then retry")
	}
	items := make([]selector.Item, 0, len(candidates))
	for i, candidate := range candidates {
		items = append(items, selector.Item{ID: fmt.Sprint(i), Title: candidate.DisplayName, Description: candidate.Provider + " · branch " + candidate.DefaultBranch})
	}
	choice, err := chooseHomeAction(command, "Add configuration repository", items)
	if err != nil {
		return api.ConfigRepository{}, err
	}
	var candidate api.ConfigRepositoryCandidate
	found := false
	for i, item := range items {
		if item.ID == choice.ID {
			candidate = candidates[i]
			found = true
			break
		}
	}
	if !found {
		return api.ConfigRepository{}, selector.ErrCanceled
	}
	confirmed, err := prompt.Confirm(prompt.ConfirmOptions{Context: command.Context(), Title: "Connect " + candidate.DisplayName + "?", Description: "Make this repository available for configuration sync. No machine files are synchronized until you enable an assignment.", Output: command.ErrOrStderr()})
	if err != nil {
		return api.ConfigRepository{}, err
	}
	if !confirmed {
		return api.ConfigRepository{}, prompt.ErrCanceled
	}
	var repository api.ConfigRepository
	err = homeLoading(command, "Add configuration repository", "Connecting repository", func(ctx context.Context) error {
		var connectErr error
		repository, connectErr = client.ConnectConfigRepository(ctx, candidate)
		return connectErr
	})
	return repository, friendlyCommandError(err)
}

func reviewConfigSyncInteractive(command *cobra.Command, client *api.Client, machineID string) error {
	var status api.ConfigSyncStatus
	err := homeLoading(command, "Review configuration updates", "Loading pending changes", func(ctx context.Context) error {
		var loadErr error
		status, loadErr = client.ConfigSyncStatus(ctx)
		return loadErr
	})
	if err != nil {
		return friendlyCommandError(err)
	}
	var pending *api.ConfigSyncEnvironmentState
	for i := range status.Environments {
		if status.Environments[i].MachineID == machineID {
			pending = &status.Environments[i]
			break
		}
	}
	if pending == nil || pending.RemoteRevision == "" || pending.RemoteRevision == pending.LastAppliedRevision || pending.AssignmentVersion < 1 {
		return showHomeText(command, "Configuration updates", "No repository update is waiting for approval on this machine.")
	}
	var review strings.Builder
	fmt.Fprintf(&review, "Revision %s\n", pending.RemoteRevision)
	for _, change := range pending.Review {
		fmt.Fprintf(&review, "%s · %s\n", change.Reason, change.Path)
	}
	if err := showHomeText(command, "Review configuration changes", review.String()); err != nil && !errors.Is(err, selector.ErrCanceled) {
		return err
	}
	confirmed, err := prompt.Confirm(prompt.ConfirmOptions{Context: command.Context(), Title: "Approve this reviewed update?", Description: "Apply revision " + pending.RemoteRevision + " within this machine's assigned configuration scope.", Output: command.ErrOrStderr()})
	if err != nil {
		return err
	}
	if !confirmed {
		return prompt.ErrCanceled
	}
	err = homeLoading(command, "Configuration updates", "Approving reviewed revision", func(ctx context.Context) error {
		_, approveErr := client.ApproveConfigPullRevision(ctx, machineID, pending.RemoteRevision, pending.AssignmentVersion)
		return approveErr
	})
	if err != nil {
		return friendlyCommandError(err)
	}
	return showHomeText(command, "Configuration updates", "The reviewed revision was approved. The worker will apply it; open Status to check progress or conflicts.")
}

func connectGitHubInteractive(command *cobra.Command, client *api.Client) (resultErr error) {
	var link api.GitHubNativeLink
	err := homeLoading(command, "Connect GitHub", "Preparing browser authorization", func(ctx context.Context) error {
		var startErr error
		link, startErr = client.StartGitHubNativeLink(ctx)
		return startErr
	})
	completed := false
	defer func() {
		if completed || link.ID == "" {
			return
		}
		ctx, cancel := githubLinkCleanupContext(command.Context())
		defer cancel()
		state, err := client.CancelGitHubNativeLink(ctx, link.ID)
		resultErr = joinGitHubLinkCancelFailure(resultErr, err)
		if err == nil && state.State == "completed" && interactiveCanceled(resultErr) {
			resultErr = errors.New("GitHub finished connecting before cancellation; return to Add repository to use the connection")
		}
	}()
	if err != nil {
		return friendlyCommandError(err)
	}
	choice, err := chooseHomeAction(command, "Approve GitHub access", []selector.Item{
		{ID: "browser", Title: "Open browser on this machine", Description: "Approve access to your GitHub account"},
		{ID: "link", Title: "Use a browser on another machine", Description: "Show a one-use link for remote or headless terminals"},
	})
	if err != nil {
		return err
	}
	if choice.ID == "link" || openGitHubAuthorizationBrowser(command.Context(), link.BrowserURL) != nil {
		// Deliberately show the one-use browser handoff only in this interactive
		// screen. Never include it in errors, diagnostics or logs.
		if err = showHomeText(command, "Connect GitHub in your browser", "Open this one-use link in your browser, check the Paperboat account, and approve GitHub access. Then press Esc to return and wait for completion.\n\n"+link.BrowserURL); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithDeadline(command.Context(), link.ExpiresAt)
	defer cancel()
	previous := command.Context()
	command.SetContext(ctx)
	defer command.SetContext(previous)
	err = homeLoading(command, "Connect GitHub", "Approve GitHub access in the browser · Esc cancels", func(ctx context.Context) error {
		return waitForGitHubNativeLink(ctx, client, link)
	})
	if err != nil {
		return err
	}
	completed = true
	return showHomeText(command, "GitHub connected", "Your provider connection is ready. Choose Add repository to select a configuration repository.")
}

func waitForGitHubNativeLink(ctx context.Context, client *api.Client, link api.GitHubNativeLink) error {
	interval := time.Duration(link.PollIntervalSeconds) * time.Second
	if interval < time.Second || interval > 10*time.Second {
		interval = 2 * time.Second
	}
	for {
		state, err := client.GitHubNativeLinkStatus(ctx, link.ID)
		if err != nil {
			return friendlyCommandError(err)
		}
		if state.ID != link.ID {
			return errors.New("control plane returned a different GitHub connection operation")
		}
		switch state.State {
		case "completed":
			return nil
		case "failed":
			return errors.New("GitHub authorization failed; retry the connection and approve access in the browser")
		case "canceled":
			return prompt.ErrCanceled
		case "expired":
			return errors.New("GitHub authorization expired; choose Connect GitHub to start again")
		case "pending", "authorizing", "exchanging":
		default:
			return errors.New("control plane returned an unknown GitHub connection state")
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return errors.New("GitHub authorization expired; choose Connect GitHub to start again")
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func configRuleText(command *cobra.Command, title, description, initial string) (string, error) {
	return prompt.Text(prompt.TextOptions{Context: command.Context(), Title: title, Description: description, Initial: initial, Stdin: os.Stdin, Output: command.ErrOrStderr()})
}

func connectCustomConfigRepositoryInteractive(command *cobra.Command, client *api.Client) (api.ConfigRepository, error) {
	endpoint, err := configRuleText(command, "Custom Git repository", "HTTPS, SSH URL or absolute local bare repository path. Do not include passwords or tokens.", "")
	if err != nil {
		return api.ConfigRepository{}, err
	}
	name, err := configRuleText(command, "Repository name", "Display name", "")
	if err != nil {
		return api.ConfigRepository{}, err
	}
	branch, err := configRuleText(command, "Repository branch", "Exact Git branch", "main")
	if err != nil {
		return api.ConfigRepository{}, err
	}
	repository, err := client.ConnectCustomConfigRepository(command.Context(), endpoint, name, branch)
	if err != nil {
		return api.ConfigRepository{}, friendlyCommandError(err)
	}
	choice, err := chooseHomeAction(command, "Repository connected", []selector.Item{{ID: "credentials", Title: "Set up credentials on this machine", Description: "Private HTTPS authentication or SSH key/agent with verified host keys"}, {ID: "later", Title: "Configure credentials later", Description: "Use pb config repository credentials set; explicit path rules are still required"}})
	if err == nil && choice.ID == "credentials" {
		err = configureRepositoryCredentialsInteractive(command, repository)
	}
	return repository, err
}

func reviewConfigConflictsInteractive(command *cobra.Command, client *api.Client, machineID string) error {
	for {
		status, err := client.ConfigSyncStatus(command.Context())
		if err != nil {
			return friendlyCommandError(err)
		}
		var rows []selector.Item
		for _, environment := range status.Environments {
			if environment.MachineID == machineID {
				for _, conflict := range environment.Conflicts {
					rows = append(rows, selector.Item{ID: conflict.Path, Title: conflict.Path, Description: strings.ReplaceAll(conflict.Reason, "_", " ")})
				}
			}
		}
		if len(rows) == 0 {
			return showHomeText(command, "Configuration conflicts", "This machine has no current configuration conflicts.")
		}
		selected, err := chooseHomeAction(command, "Configuration conflicts", rows)
		if err != nil {
			return err
		}
		action, err := chooseHomeAction(command, selected.Title, []selector.Item{{ID: "compare", Title: "Compare versions", Description: "Read this machine and repository content without changing files"}, {ID: "machine", Title: "Keep this machine's version", Action: true}, {ID: "repository", Title: "Use repository version", Action: true}})
		if err != nil {
			if errors.Is(err, selector.ErrCanceled) {
				continue
			}
			return err
		}
		args := []string{"config", "conflict", "compare", machineID, selected.ID}
		if action.ID != "compare" {
			approved, err := prompt.Confirm(prompt.ConfirmOptions{Context: command.Context(), Title: "Resolve " + selected.ID, Description: "Keep the " + action.ID + " version?", Stdin: os.Stdin, Output: command.ErrOrStderr()})
			if err != nil {
				return err
			}
			if !approved {
				continue
			}
			args = []string{"config", "conflict", "resolve", machineID, selected.ID, "--keep", action.ID}
		}
		if err := runHomeResult(command, args); err != nil {
			if showErr := showHomeFailure(command, err); showErr != nil {
				return showErr
			}
		}
	}
}
