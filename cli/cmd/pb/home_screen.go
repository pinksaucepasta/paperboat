package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/fileindex"
	"github.com/pinksaucepasta/paperboat/internal/preferences"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func actionHome(command *cobra.Command) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return commandRejection{reason: commandRejectNoninteractiveHome}
	}
	previousContext := command.Context()
	homeContext, cancelHome := context.WithCancel(previousContext)
	command.SetContext(homeContext)
	defer func() { cancelHome(); command.SetContext(previousContext) }()
	endScreen := selector.BeginScreen(command.ErrOrStderr())
	defer endScreen()
	if prefetch, prefetchErr := startHomePrefetch(command); prefetchErr == nil {
		command.SetContext(context.WithValue(command.Context(), homePrefetchContextKey{}, prefetch))
	}
	primeHomeFileIndex(command.Context())
	revealEmail := false
	for {
		emailAction := "ctrl+e show email"
		if revealEmail {
			emailAction = "ctrl+e hide email"
		}
		selection, err := selector.ChooseWithAction(selector.Options{
			Header:  homeBrand(command, revealEmail),
			Title:   "What do you want to do?",
			Stdin:   os.Stdin,
			Context: command.Context(), Output: command.ErrOrStderr(),
			Footer:        "↑/↓ move  enter/click select  " + emailAction + "  esc exit",
			Actions:       map[string]string{"ctrl+e": "toggle-email"},
			HeaderActions: map[int]string{2: "toggle-email"},
			Items:         personalizedHomeItems(preferences.FromContext(command.Context())),
		})
		if err != nil {
			if errors.Is(err, selector.ErrCanceled) || errors.Is(err, selector.ErrInterrupted) {
				return nil
			}
			return err
		}
		if selection.Action == "toggle-email" {
			revealEmail = !revealEmail
			continue
		}
		err = runHomeAction(command, selection.Item.ID)
		if errors.Is(err, selector.ErrInterrupted) {
			return nil
		}
		if interactiveCanceled(err) || err == nil {
			continue
		}
		if displayErr := showHomeFailure(command, err); displayErr != nil {
			return displayErr
		}
	}
}

const homePrefetchFreshness = 5 * time.Second

type homePrefetchContextKey struct{}

type asyncHomeValue[T any] struct {
	done      chan struct{}
	value     T
	err       error
	fetchedAt time.Time
}

func startAsyncHomeValue[T any](ctx context.Context, load func(context.Context) (T, error)) *asyncHomeValue[T] {
	value := &asyncHomeValue[T]{done: make(chan struct{})}
	go func() {
		defer close(value.done)
		value.value, value.err = load(ctx)
		value.fetchedAt = time.Now()
	}()
	return value
}

func (v *asyncHomeValue[T]) ready() bool {
	select {
	case <-v.done:
		return true
	default:
		return false
	}
}

func (v *asyncHomeValue[T]) await(ctx context.Context) (T, error) {
	select {
	case <-v.done:
		return v.value, v.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

func (v *asyncHomeValue[T]) fresh() bool {
	return v.ready() && v.err == nil && time.Since(v.fetchedAt) <= homePrefetchFreshness
}

type homePrefetch struct {
	favorites *asyncHomeValue[favoriteSet]
	machines  *asyncHomeValue[[]api.UserMachine]
	sessions  *asyncHomeValue[[]machineSession]

	machinesClaimed atomic.Bool
	sessionsClaimed atomic.Bool
}

func startHomePrefetch(command *cobra.Command) (*homePrefetch, error) {
	client, err := backendForCommand(command)
	if err != nil {
		return nil, err
	}
	ctx := command.Context()
	prefetch := &homePrefetch{
		favorites: startAsyncHomeValue(ctx, func(ctx context.Context) (favoriteSet, error) { return loadFavorites(ctx, client) }),
		machines:  startAsyncHomeValue(ctx, client.ListUserMachines),
	}
	prefetch.sessions = startAsyncHomeValue(ctx, func(ctx context.Context) ([]machineSession, error) {
		machines, loadErr := prefetch.machines.await(ctx)
		if loadErr != nil {
			return nil, loadErr
		}
		return listMachineSessionsForMachines(ctx, client, machines)
	})
	return prefetch, nil
}

func homePrefetchFor(command *cobra.Command) *homePrefetch {
	value, _ := command.Context().Value(homePrefetchContextKey{}).(*homePrefetch)
	return value
}

func primeHomeFileIndex(ctx context.Context) {
	go func() {
		root, rootErr := os.UserHomeDir()
		cachePath, cacheErr := fileindex.CachePath()
		if err := errors.Join(rootErr, cacheErr); err != nil {
			errorreport.Current().ObserveFailure(ctx, "paperboat-cli", "send", "command", "command_failed", err)
			return
		}
		fileindex.RefreshInBackground(ctx, root, cachePath)
	}()
}

func runHomeAction(command *cobra.Command, action string) error {
	if strings.HasPrefix(action, "shortcut:") {
		return runPreferenceFavorite(command, strings.TrimPrefix(action, "shortcut:"))
	}
	switch action {
	case "customize":
		return editPreferences(command)
	case "previews":
		return actionHomePreviews(command)
	case "commands":
		return actionHomeCommands(command, nil)
	case "team":
		return actionHomeTeams(command)
	case "send":
		return actionHomeSentFiles(command)
	case "sessions":
		return actionHomeSessions(command)
	case "environment-variables":
		return runEnvironmentVariablesTUI(command)
	case "machines":
		return actionHomeMachines(command)
	case "inbox":
		return actionHomeInbox(command)
	case "config":
		return actionHomeConfig(command)
	case "doctor":
		return actionHomeDoctor(command)
	case "account":
		return actionHomeAccount(command)
	case "switch-workspace":
		selected, err := selectWorkspace(command, nil)
		if err != nil {
			return err
		}
		if !selected {
			return selector.ErrCanceled
		}
		return selector.ErrInterrupted
	default:
		return errors.New("unknown Paperboat action")
	}
}

func actionHomeInbox(command *cobra.Command) error {
	ctx := actionContext(command, nil)
	client, err := backendClient(ctx)
	if err != nil {
		return err
	}
	me, err := client.Me(ctx.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	for {
		requests, listErr := client.PendingTeamInboxRequests(ctx.Context)
		if listErr != nil {
			return friendlyCommandError(listErr)
		}
		pending := slices.DeleteFunc(requests, func(request api.TeamInboxRequest) bool {
			return request.RecipientAccount != me.ID || request.Status != "pending"
		})
		if len(pending) == 0 {
			return showInformation(command, "Team Inbox", "No file requests are waiting for your approval.", nil)
		}
		items := make([]selector.Item, len(pending))
		for i, request := range pending {
			items[i] = selector.Item{ID: request.RequestID, Title: fmt.Sprintf("%d file(s) from %s", len(request.Files), request.SenderAccount), Description: "Expires " + relativeTimestamp(request.ExpiresAt)}
		}
		selection, selectErr := selector.ChooseWithAction(selector.Options{Title: "Team Inbox approvals", Subtitle: "Approval applies only to the listed names, sizes, hashes, recipient and transfer", Items: items, Footer: "enter review  esc back", Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
		if selectErr != nil {
			return selectErr
		}
		request := pending[slices.IndexFunc(pending, func(request api.TeamInboxRequest) bool { return request.RequestID == selection.Item.ID })]
		lines := make([]string, len(request.Files))
		for i, file := range request.Files {
			lines[i] = fmt.Sprintf("%s — %d bytes — SHA-256 %s", file.Basename, file.Size, file.SHA256)
		}
		action, actionErr := chooseHomeAction(command, "File request from "+request.SenderAccount, []selector.Item{{ID: "approve", Title: "Approve exact files", Description: strings.Join(lines, "; ")}, {ID: "decline", Title: "Decline", Description: "No file bytes will be accepted"}})
		if actionErr != nil {
			if errors.Is(actionErr, selector.ErrCanceled) {
				continue
			}
			return actionErr
		}
		if _, err = client.DecideTeamInboxRequest(ctx.Context, request.RequestID, action.ID, request.DecisionGeneration); err != nil {
			return friendlyCommandError(err)
		}
	}
}

func versionDisplay(version string) string {
	return brandDisplay(version, "") + "\n"
}

func homeBrand(command *cobra.Command, revealEmail bool) string {
	account := "Not signed in"
	configPath, _ := command.Flags().GetString("config")
	cfg, err := config.Load(configPath)
	if err == nil {
		if server, _ := command.Flags().GetString("server"); strings.TrimSpace(server) != "" {
			cfg.ServerURL, err = config.NormalizeServerURL(server)
		}
	}
	if err == nil && cfg.ServerURL != "" {
		if store, storeErr := config.ProfileStoreFor(cfg); storeErr == nil {
			if profile, profileErr := store.Load(cfg.ServerURL); profileErr == nil {
				account = firstNonEmpty(profile.Account.Email, profile.Account.DisplayName, profile.Account.ID, account)
			}
		}
	}
	if !revealEmail {
		account = maskEmail(account)
	}
	if workspace, workspaceErr := effectiveWorkspace(command); workspaceErr == nil && workspace != "" {
		account += " · " + workspaceDisplayName(workspace)
	}
	return brandDisplay(buildinfo.Version, account)
}

func workspaceDisplayName(selector string) string {
	if selector == "personal" {
		return "Personal workspace"
	}
	return selector + " workspace"
}

func maskEmail(value string) string {
	local, domain, ok := strings.Cut(value, "@")
	if !ok {
		return value
	}
	maskPart := func(part string) string {
		return strings.Repeat("█", len([]rune(part)))
	}
	domainParts := strings.Split(domain, ".")
	for index, part := range domainParts {
		domainParts[index] = maskPart(part)
	}
	return maskPart(local) + "@" + strings.Join(domainParts, ".")
}

func brandDisplay(version, account string) string {
	art := []string{"      ▄█▄", "  ▄▄▝▀▀▀▀▀▘▄▄", "   ▀███████▀"}
	details := []string{"Paperboat", "Version " + version, account}
	artWidth := 0
	for _, line := range art {
		artWidth = max(artWidth, ansi.StringWidth(line))
	}
	lines := make([]string, len(art))
	for index, line := range art {
		lines[index] = line
		if details[index] != "" {
			lines[index] += strings.Repeat(" ", artWidth-ansi.StringWidth(line)+3) + details[index]
		}
	}
	return strings.Join(lines, "\n")
}

func backendForCommand(command *cobra.Command) (*api.Client, error) {
	ctx := actionContext(command, nil)
	d, err := buildDeps(ctx)
	if err != nil {
		return nil, err
	}
	return newWorkspaceAPIClient(ctx, d.cfg.ServerURL, d.auth)
}

func executeInteractiveCommand(parent *cobra.Command, args []string) error {
	command := newInteractiveRootCommand()
	command.SetIn(parent.InOrStdin())
	command.SetOut(parent.OutOrStdout())
	command.SetErr(parent.ErrOrStderr())
	resolved, ctx, err := preparePreferences(command, interactiveArgs(parent, args), parent.Context())
	if err != nil {
		return err
	}
	command.SetArgs(resolved)
	restore := selector.SuspendScreen(parent.ErrOrStderr())
	defer restore()
	return command.ExecuteContext(ctx)
}
