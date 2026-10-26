package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localwait"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"golang.org/x/term"
)

type environmentTarget struct {
	kind string
	id   string
	name string
}

const environmentUserMachine = "machine"

func selectEnvironment(ctx context.Context, client *api.Client, title string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("an environment is required in non-interactive use")
	}
	var machines []api.UserMachine
	load := func(loadCtx context.Context) error {
		var err error
		machines, err = client.ListUserMachines(loadCtx)
		return friendlyCommandError(err)
	}
	var err error
	if selector.ScreenActive() {
		err = selector.Loading(ctx, title, "Loading environments", os.Stdin, os.Stderr, load)
	} else {
		err = load(ctx)
	}
	if err != nil {
		return "", err
	}
	machines = terminalHostMachines(machines)
	items := make([]selector.Item, 0, len(machines))
	for _, machine := range machines {
		if !machine.Capabilities.TerminalHost.Configured || !machine.Capabilities.TerminalHost.Observed {
			continue
		}
		items = append(items, selector.Item{ID: machine.ID, Title: machine.Alias, Description: preferenceDetails(ctx, "machines", map[string]string{"status": machineStatusSummary(machine), "platform": machine.Platform, "id": machine.ID}), Search: "machine " + machineStatusSearch(machine)})
	}
	selected, err := selector.Choose(selector.Options{Context: ctx, Title: title, Subtitle: "Terminal-capable machines", Items: items, Empty: "no terminal-capable machines are available; run `pb setup` or `pb machine add`", Stdin: os.Stdin, Output: os.Stderr})
	return selected.ID, err
}

func selectSession(ctx context.Context, client *api.Client, target environmentTarget, sessions []api.TerminalSession, title string) (api.TerminalSession, error) {
	items := make([]selector.Item, 0, len(sessions))
	byID := make(map[string]api.TerminalSession, len(sessions))
	for _, session := range sessions {
		attached := "no attachments"
		if session.AttachedCount != nil {
			attached = fmt.Sprintf("%d attached", *session.AttachedCount)
		}
		activity := "last active " + relativeTime(session.LastActiveAt)
		created := "created " + relativeTimestamp(session.CreatedAt)
		description := sessionIdentificationDetails(session, preferenceDetails(ctx, "sessions", map[string]string{"machine": target.name, "state": strings.Join([]string{session.State, attached, activity, created}, " · "), "id": session.ID}))
		items = append(items, selector.Item{ID: session.ID, Title: session.Name, Description: description, Search: target.name + " " + session.State + " " + sessionIdentificationSearch(session)})
		byID[session.ID] = session
	}
	refresh := func(refreshCtx context.Context) ([]selector.Item, error) {
		fresh, err := listTerminalSessionsForTarget(refreshCtx, client, target)
		if err != nil {
			return nil, err
		}
		updated := make([]selector.Item, 0, len(fresh))
		for _, entry := range fresh {
			if _, ok := byID[entry.ID]; !ok {
				continue
			}
			updated = append(updated, selector.Item{ID: entry.ID, Title: entry.Name, Description: sessionIdentificationDetails(entry, target.name+" · "+entry.State), Search: sessionIdentificationSearch(entry)})
		}
		return updated, nil
	}
	selected, err := selector.Choose(selector.Options{Context: ctx, Refresh: refresh, Title: title, Subtitle: target.name + "  ·  " + target.kind, Items: items, Empty: "no terminal sessions are available", Stdin: os.Stdin, Output: os.Stderr})
	return byID[selected.ID], err
}

func defaultEnvironment(ctx context.Context, client *api.Client, rememberedID, workspace string) (string, error) {
	machines, err := client.ListUserMachines(ctx)
	if err != nil {
		return "", friendlyCommandError(err)
	}
	return defaultEnvironmentFrom(machines, rememberedID, workspace)
}

// defaultEnvironmentFrom chooses the terminal machine to use when none was named, from machines
// the caller already fetched.
func defaultEnvironmentFrom(machines []api.UserMachine, rememberedID, workspace string) (string, error) {
	machines = terminalHostMachines(machines)
	if rememberedID = strings.TrimSpace(rememberedID); rememberedID != "" {
		for _, machine := range machines {
			if machine.ID == rememberedID {
				return rememberedID, nil
			}
		}
		return "", fmt.Errorf("last environment %q is not available in the selected %s workspace; choose a machine in this workspace or run `pb switch <workspace>`", rememberedID, workspaceDisplayName(workspace))
	}
	if len(machines) == 1 {
		return machines[0].ID, nil
	}
	if len(machines) == 0 {
		return "", errors.New("no terminal-capable machines are available; run `pb setup` or `pb machine add`")
	}
	choices := make([]string, 0, len(machines))
	for _, machine := range machines {
		choices = append(choices, fmt.Sprintf("%s (%s)", machine.Alias, machine.ID))
	}
	return "", fmt.Errorf("multiple environments are available: %s; choose one with `pb <environment>`", strings.Join(choices, ", "))
}

func defaultWorkspaceEnvironment(ctx context.Context, commandContext *command.Context, client *api.Client, cfg *config.Config) (string, error) {
	machines, err := client.ListUserMachines(ctx)
	if err != nil {
		return "", friendlyCommandError(err)
	}
	return defaultWorkspaceEnvironmentFrom(commandContext, client, cfg, machines)
}

// defaultWorkspaceEnvironmentFrom is defaultWorkspaceEnvironment over machines already fetched, so a
// command that then resolves the chosen machine does not list them a second time.
func defaultWorkspaceEnvironmentFrom(commandContext *command.Context, client *api.Client, cfg *config.Config, machines []api.UserMachine) (string, error) {
	workspace := client.Workspace()
	if workspace == "" {
		var err error
		workspace, err = workspaceForCommandContext(commandContext)
		if err != nil {
			return "", err
		}
	}
	accountID, err := workspaceAccountID(cfg)
	if err != nil {
		return "", err
	}
	rememberedID := ""
	if accountID != "" {
		rememberedID, err = workspaceLastEnvironment(cfg, cfg.ServerURL, accountID, workspace)
		if err != nil {
			return "", err
		}
	}
	return defaultEnvironmentFrom(machines, rememberedID, workspace)
}

func resolveEnvironmentTarget(ctx context.Context, client *api.Client, requested string) (environmentTarget, error) {
	machine, err := resolveUserMachine(ctx, client, requested)
	if err != nil {
		return environmentTarget{}, err
	}
	return environmentTarget{kind: environmentUserMachine, id: machine.ID, name: machine.Alias}, nil
}

func resolveTerminalEnvironmentTarget(ctx context.Context, client *api.Client, requested string) (environmentTarget, error) {
	machine, err := resolveUserMachine(ctx, client, requested)
	if err != nil {
		return environmentTarget{}, err
	}
	if err := terminalHostError(machine); err != nil {
		return environmentTarget{}, err
	}
	return environmentTarget{kind: environmentUserMachine, id: machine.ID, name: machine.Alias}, nil
}

// resolveTerminalTargetOrDefault names the terminal machine a command works on: the one requested, or
// the default for the workspace, using a single listing of the account's machines for both steps.
func resolveTerminalTargetOrDefault(ctx context.Context, commandContext *command.Context, client *api.Client, cfg *config.Config, requested string) (environmentTarget, error) {
	machines, err := client.ListUserMachines(ctx)
	if err != nil {
		return environmentTarget{}, friendlyCommandError(err)
	}
	if requested == "" {
		if requested, err = defaultWorkspaceEnvironmentFrom(commandContext, client, cfg, machines); err != nil {
			return environmentTarget{}, err
		}
	}
	machine, err := resolveUserMachineIn(machines, requested)
	if err != nil {
		return environmentTarget{}, err
	}
	if err := terminalHostError(machine); err != nil {
		return environmentTarget{}, err
	}
	return environmentTarget{kind: environmentUserMachine, id: machine.ID, name: machine.Alias}, nil
}

func terminalHostMachines(machines []api.UserMachine) []api.UserMachine {
	eligible := make([]api.UserMachine, 0, len(machines))
	for _, machine := range machines {
		if terminalHostError(machine) == nil {
			eligible = append(eligible, machine)
		}
	}
	return eligible
}

func terminalHostError(machine api.UserMachine) error {
	if !machine.Capabilities.TerminalHost.Configured {
		return &api.APIError{Code: "machine_capability_unavailable", Message: "This machine is not configured to host terminals."}
	}
	if !machine.Online || !machine.Capabilities.TerminalHost.Observed {
		return &api.APIError{Code: "machine_offline", Message: "This terminal host is offline."}
	}
	return nil
}

func listTerminalSessionsForTarget(ctx context.Context, client *api.Client, target environmentTarget) ([]api.TerminalSession, error) {
	return client.ListUserMachineTerminalSessions(ctx, target.id)
}

func createTerminalSessionForTarget(ctx context.Context, client *api.Client, target environmentTarget, name, idempotencyKey string) (api.TerminalSession, error) {
	return client.CreateUserMachineTerminalSession(ctx, target.id, name, idempotencyKey)
}

func renameTerminalSessionForTarget(ctx context.Context, client *api.Client, target environmentTarget, sessionID, name string) (api.TerminalSession, error) {
	return client.RenameUserMachineTerminalSession(ctx, target.id, sessionID, name)
}

func closeTerminalSessionForTarget(ctx context.Context, client *api.Client, target environmentTarget, sessionID string) (api.TerminalSessionCloseResult, error) {
	return client.CloseUserMachineTerminalSession(ctx, target.id, sessionID)
}

func deleteTerminalSessionForTarget(ctx context.Context, client *api.Client, target environmentTarget, sessionID string) error {
	return client.DeleteUserMachineTerminalSession(ctx, target.id, sessionID)
}

func selectTerminalSession(ctx context.Context, client *api.Client, projectRef, name, ref string) (api.TerminalSession, environmentTarget, api.UserMachine, error) {
	target, machine, err := resolveEnvironmentTargetWithMachine(ctx, client, projectRef)
	if err != nil {
		return api.TerminalSession{}, environmentTarget{}, api.UserMachine{}, err
	}
	if strings.TrimSpace(ref) == "" {
		if err := validateSessionNameOptional(name); err != nil {
			return api.TerminalSession{}, environmentTarget{}, api.UserMachine{}, err
		}
		session, err := createTerminalSessionForTarget(ctx, client, target, name, newIdempotencyKey())
		if err != nil {
			return api.TerminalSession{}, environmentTarget{}, api.UserMachine{}, friendlyCommandError(err)
		}
		if session.EvictedSession != nil {
			fmt.Fprintf(os.Stderr, "Session limit reached; removed least-recent session %q (%s).\n", session.EvictedSession.Name, session.EvictedSession.State)
		}
		return session, target, machine, nil
	}
	session, err := resolveTerminalSession(ctx, client, target, ref)
	if err != nil {
		return api.TerminalSession{}, environmentTarget{}, api.UserMachine{}, friendlyCommandError(err)
	}
	return session, target, machine, nil
}

// resolveEnvironmentTargetWithMachine resolves like resolveEnvironmentTarget but
// also returns the machine catalog entry for machine targets so callers can
// reuse it without a second listing round trip. The daemon's warm inventory
// snapshot is consulted first: a machine it already tracks needs no catalog
// listing at all, and the server revalidates the machine on every session
// create and connection descriptor anyway.
func resolveEnvironmentTargetWithMachine(ctx context.Context, client *api.Client, requested string) (environmentTarget, api.UserMachine, error) {
	if machine, ok := resolveWarmUserMachine(ctx, requested); ok && machine.InstallationGeneration > 0 {
		return environmentTarget{kind: environmentUserMachine, id: machine.ID, name: machine.Alias}, machine, nil
	}
	machine, err := resolveUserMachine(ctx, client, requested)
	if err != nil {
		return environmentTarget{}, api.UserMachine{}, err
	}
	return environmentTarget{kind: environmentUserMachine, id: machine.ID, name: machine.Alias}, machine, nil
}

func resolveUserMachine(ctx context.Context, client *api.Client, requested string) (api.UserMachine, error) {
	// The server filters by a case-insensitive substring of name and ID, so a machine named or
	// identified exactly by requested is always in this page; the account is not listed in full
	// (Lane X audit A2: ~50 serial pages at 10,000 machines).
	machines, err := client.ListUserMachinesFiltered(ctx, url.Values{"q": {requested}})
	if err != nil {
		return api.UserMachine{}, err
	}
	machine, err := resolveUserMachineIn(machines, requested)
	if err != nil {
		return resolveInOtherWorkspaces(ctx, client, requested, err)
	}
	return machine, nil
}

// resolveUserMachineIn finds the one machine a user named, by ID or by name, among machines already fetched.
func resolveUserMachineIn(machines []api.UserMachine, requested string) (api.UserMachine, error) {
	for _, machine := range machines {
		if machine.ID == requested {
			return machine, nil
		}
	}
	var matches []api.UserMachine
	for _, machine := range machines {
		if strings.EqualFold(machine.Alias, requested) {
			matches = append(matches, machine)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, machine := range matches {
			ids = append(ids, machine.ID)
		}
		return api.UserMachine{}, fmt.Errorf("%w: %q matches machine IDs %s; use an exact ID", resolver.ErrMachineAmbiguous, requested, strings.Join(ids, ", "))
	}
	return api.UserMachine{}, fmt.Errorf("%w: %q", resolver.ErrMachineNotFound, requested)
}

// resolveWarmUserMachine uses the daemon's authenticated inventory snapshot
// for selection. Operation descriptors remain fetched from the control plane;
// this only removes a redundant catalog round trip on an already-warm client.
var loadWarmMachineSnapshot = func(ctx context.Context) (localapi.Snapshot, error) {
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		return localapi.Snapshot{}, err
	}
	client, err := localapi.NewClient(paths.SocketPath, 300*time.Millisecond)
	if err != nil {
		return localapi.Snapshot{}, err
	}
	return client.Snapshot(ctx)
}

// workspaceSnapshotMachines selects the daemon snapshot's machines that belong to the
// invocation's workspace. It reports false when only the server can answer: another workspace's
// membership is not in the snapshot, while the personal workspace is the account's own personal
// machines, which the snapshot does carry.
func workspaceSnapshotMachines(ctx context.Context, machines []localapi.MachineStatus) ([]localapi.MachineStatus, bool) {
	selected, scoped := workspaceFromContext(ctx)
	if !scoped {
		return machines, true
	}
	account, ok := ctx.Value(personalWorkspaceAccountKey{}).(string)
	if selected != "personal" || !ok || account == "" {
		return nil, false
	}
	personal := make([]localapi.MachineStatus, 0, len(machines))
	for _, machine := range machines {
		if machine.Ownership == "personal" && machine.OwnerAccount == account {
			personal = append(personal, machine)
		}
	}
	return personal, true
}

// resolveMachineWarmFirst resolves a machine from the daemon's warm snapshot and lists the
// account's machines on the server only when the snapshot cannot answer.
func resolveMachineWarmFirst(ctx context.Context, requested string, server func() (*api.Client, error)) (api.UserMachine, error) {
	if machine, ok := resolveWarmUserMachine(ctx, requested); ok {
		return machine, nil
	}
	client, err := server()
	if err != nil {
		return api.UserMachine{}, err
	}
	return resolveUserMachine(ctx, client, requested)
}

func resolveWarmUserMachine(ctx context.Context, requested string) (api.UserMachine, bool) {
	if _, local := workspaceSnapshotMachines(ctx, nil); !local {
		return api.UserMachine{}, false
	}
	snapshot, err := loadWarmMachineSnapshot(ctx)
	if err != nil {
		return api.UserMachine{}, false
	}
	candidates, _ := workspaceSnapshotMachines(ctx, snapshot.Machines)
	status, err := localwait.ResolveMachine(candidates, requested)
	// Only a machine the snapshot sees ready is taken from it. Presence in the snapshot can lag (a just-paired
	// peer stays cached offline until the daemon's next refresh), so anything else is resolved on the server:
	// a cached "offline" never refuses a command.
	if err != nil || !status.Eligible || status.Generation == 0 || status.RuntimeState != "ready" {
		return api.UserMachine{}, false
	}
	return api.UserMachine{ID: status.ID, EnvironmentID: status.EnvironmentID, WorkspaceRoot: status.WorkspaceRoot, Alias: status.Alias, Platform: status.Platform, State: "ready", Online: true, InstallationGeneration: int64(status.Generation), SSHUser: status.SSHUser, SSHPort: status.SSHPort}, true
}

func resolveTerminalSession(ctx context.Context, client *api.Client, target environmentTarget, ref string) (api.TerminalSession, error) {
	sessions, err := listTerminalSessionsForTarget(ctx, client, target)
	if err != nil {
		return api.TerminalSession{}, friendlyCommandError(err)
	}
	for _, s := range sessions {
		if s.ID == ref || strings.EqualFold(s.Name, ref) {
			return s, nil
		}
	}
	var suggestions []string
	for _, s := range sessions {
		if strings.HasPrefix(strings.ToLower(s.Name), strings.ToLower(ref)) || editDistance(strings.ToLower(s.Name), strings.ToLower(ref)) <= 2 {
			suggestions = append(suggestions, s.Name)
			if len(suggestions) == 3 {
				break
			}
		}
	}
	message := fmt.Sprintf("terminal session %q was not found", ref)
	if len(suggestions) > 0 {
		message += "; did you mean " + strings.Join(suggestions, ", ") + "?"
	}
	message += "; create one with `pb <environment> new`"
	return api.TerminalSession{}, errors.New(message)
}

var sessionNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

var automaticSessionNamePattern = regexp.MustCompile(`^shell-[0-9]+$`)

func validateSessionNameOptional(name string) error {
	if name == "" {
		return nil
	}
	return validateSessionName(name)
}

func validateSessionName(name string) error {
	if name == "default" || automaticSessionNamePattern.MatchString(name) || !sessionNamePattern.MatchString(name) {
		return localArgumentError("session names must be lowercase 1-64 character values matching [a-z0-9][a-z0-9._-]{0,63}; default and shell-N are reserved")
	}
	return nil
}

// Presentation belongs to the invocation owner; forwarding keeps typed
// status, attempt ownership and support references intact.
func friendlyCommandError(err error) error { return err }

func refreshSessionIdentification(ctx context.Context, client *api.Client, sessions []machineSession) ([]machineSession, error) {
	targets := make(map[string]environmentTarget)
	for _, entry := range sessions {
		targets[entry.target.id] = entry.target
	}
	var fresh []machineSession
	for _, target := range targets {
		listed, err := listTerminalSessionsForTarget(ctx, client, target)
		if err != nil {
			return nil, err
		}
		for _, entry := range listed {
			fresh = append(fresh, machineSession{target: target, session: entry})
		}
	}
	return fresh, nil
}

func machineSessionIdentificationItem(ctx context.Context, entry machineSession, favorites favoriteSet) selector.Item {
	attached := "no attachments"
	if entry.session.AttachedCount != nil {
		attached = fmt.Sprintf("%d attached", *entry.session.AttachedCount)
	}
	details := preferenceDetails(ctx, "sessions", map[string]string{"machine": entry.target.name, "state": entry.session.State + " · " + attached + " · last active " + relativeTime(entry.session.LastActiveAt), "id": entry.session.ID})
	return selector.Item{ID: entry.target.id + ":" + entry.session.ID, Title: entry.session.Name, Description: sessionIdentificationDetails(entry.session, details), Search: entry.target.name + " " + sessionIdentificationSearch(entry.session) + " favorite starred", Favorite: favorites.IsFavorite("session", machineSessionFavoriteID(entry))}
}
