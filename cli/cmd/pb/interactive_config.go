package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/prompt"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/spf13/cobra"
)

// Only guided config-sync actions use a terminal confirmation instead of the
// copy-and-repeat confirmation contract used by noninteractive commands.
type configSyncInteractiveConfirmation struct{}

func actionHomeConfigSync(command *cobra.Command) error {
	store, err := runtimeIdentityStore()
	if err != nil {
		return fmt.Errorf("inspect this machine's registration: %w", err)
	}
	registration, err := store.Registration()
	if err != nil || registration.MachineID == "" {
		return errors.New("this machine is not enrolled; complete Paperboat setup before enabling config sync")
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
		case "configure":
			err = configureSyncInteractive(command, client, machineID, assignment)
		case "disable":
			err = runConfigSyncResult(command, []string{"config", "unassign", machineID})
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
			selector.Item{ID: "review", Title: "Review pending updates", Description: "Inspect changes before approving a repository revision"},
			selector.Item{ID: "repair", Title: "Start or repair worker", Description: "Retry local worker installation without changing repositories"},
			selector.Item{ID: "disable", Title: "Disable sync", Description: "Stop this machine's worker and remove its assignment; keep repository content", Action: true})
	} else {
		items = append(items, selector.Item{ID: "configure", Title: "Enable sync", Description: "Choose repositories, sync direction and update behavior", Action: true})
	}
	return append(items, selector.Item{ID: "repositories", Title: "Add repository", Description: "Choose a repository from your connected provider account"}, selector.Item{ID: "github", Title: "Connect GitHub", Description: "Approve account access in your browser, then return here"})
}

func runConfigSyncResult(parent *cobra.Command, args []string) error {
	original := parent.Context()
	parent.SetContext(context.WithValue(original, configSyncInteractiveConfirmation{}, true))
	defer parent.SetContext(original)
	return runHomeResult(parent, args)
}

func configureSyncInteractive(command *cobra.Command, client *api.Client, machineID string, current api.ConfigAssignment) error {
	modeItems := []selector.Item{
		{ID: "pull-only", Title: "Pull only", Description: "Apply repository configuration to this machine"},
		{ID: "push-only", Title: "Push only", Description: "Publish selected machine configuration to the repository"},
		{ID: "bidirectional", Title: "Pull and push", Description: "Apply repository updates and publish machine changes"},
	}
	// Put the current direction first so changing a repository preserves the
	// existing direction unless the user explicitly chooses another.
	currentMode := strings.ReplaceAll(current.Mode, "_", "-")
	for i, item := range modeItems {
		if item.ID == currentMode {
			modeItems[0], modeItems[i] = modeItems[i], modeItems[0]
			break
		}
	}
	mode, err := chooseHomeAction(command, "Sync direction", modeItems)
	if err != nil {
		return err
	}
	var pull, push api.ConfigRepository
	if mode.ID != "push-only" {
		pull, err = chooseConfigRepositoryInteractive(command, client, "Pull repository", current.PullRepositoryID)
		if err != nil {
			return err
		}
	}
	if mode.ID != "pull-only" {
		push, err = chooseConfigRepositoryInteractive(command, client, "Push repository", current.PushRepositoryID)
		if err != nil {
			return err
		}
	}
	automatic := false
	if mode.ID != "push-only" {
		options := []selector.Item{
			{ID: "review", Title: "Review each update", Description: "Inspect changed paths and approve before applying later updates"},
			{ID: "automatic", Title: "Apply updates automatically", Description: "Apply later updates within the selected scope; conflicts remain visible"},
		}
		if current.AutomaticUpdates {
			options[0], options[1] = options[1], options[0]
		}
		selected, chooseErr := chooseHomeAction(command, "Repository updates", options)
		if chooseErr != nil {
			return chooseErr
		}
		automatic = selected.ID == "automatic"
	}
	repository := pull.ID
	if repository == "" {
		repository = push.ID
	}
	args := []string{"config", "assign", repository, machineID, "--mode", mode.ID}
	if pull.ID != "" {
		args = append(args, "--pull-repository", pull.ID)
	}
	if push.ID != "" {
		args = append(args, "--push-repository", push.ID)
	}
	if automatic {
		args = append(args, "--automatic-updates")
	}
	return runConfigSyncResult(command, args)
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
		items = append(items, selector.Item{ID: "add", Title: "Add repository", Description: "Choose from your connected provider account", Action: true})
		choice, err := chooseHomeAction(command, title, items)
		if err != nil {
			return api.ConfigRepository{}, err
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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		state, err := client.CancelGitHubNativeLink(ctx, link.ID)
		if err != nil {
			resultErr = errors.Join(resultErr, errors.New("the pending GitHub connection could not be canceled; it expires automatically, and no provider credentials were returned to this CLI"))
		}
		if err == nil && state.State == "completed" && interactiveCanceled(resultErr) {
			resultErr = errors.New("GitHub finished connecting before cancellation; return to Add repository to use the connection")
		}
	}()
	if err != nil {
		return friendlyCommandError(err)
	}
	choice, err := chooseHomeAction(command, "Approve GitHub access", []selector.Item{
		{ID: "browser", Title: "Open browser on this machine", Description: "Approve access to your GitHub account"},
		{ID: "link", Title: "Use a browser on another device", Description: "Show a one-use link for remote or headless terminals"},
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
