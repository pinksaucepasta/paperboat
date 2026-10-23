package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/fileindex"
	"github.com/pinksaucepasta/paperboat/internal/machinename"
	"github.com/pinksaucepasta/paperboat/internal/prompt"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/spf13/cobra"
)

func actionEnvironmentsList(command *cobra.Command) error {
	client, err := backendForCommand(command)
	if err != nil {
		return err
	}
	machines, err := client.ListUserMachinesFiltered(command.Context(), inventoryFilters(command))
	if err != nil {
		return friendlyCommandError(err)
	}
	currentMachineID, _ := configuredMachineID()
	sortMachinesForDisplay(machines, nil, currentMachineID)
	items := make([]selector.Item, 0, len(machines))
	for _, machine := range machines {
		items = append(items, selector.Item{ID: machine.ID, Title: machineDisplayTitle(machine, currentMachineID), Description: preferenceDetails(command.Context(), "machines", map[string]string{"status": machineStatusSummary(machine), "platform": machine.Platform, "id": machine.ID}), Search: machine.ID + " " + machine.WorkspaceRoot + " " + machineStatusSearch(machine)})
	}
	_, err = selector.Choose(selector.Options{Title: "Machines", Subtitle: "Computers available to this account", Items: items, Empty: "No machines yet", Footer: "↑/↓ inspect  type to filter  esc back", Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
	return err
}

func actionHomeOwnedSessions(command *cobra.Command) error {
	client, err := backendForCommand(command)
	if err != nil {
		return err
	}
	usePrefetch := true
	for {
		var favorites favoriteSet
		var sessions []machineSession
		var listErr error
		loaded := false
		if usePrefetch {
			usePrefetch = false
			if prefetch := homePrefetchFor(command); prefetch != nil && prefetch.sessionsClaimed.CompareAndSwap(false, true) {
				work := func(ctx context.Context) error {
					var loadErr error
					favorites, loadErr = prefetch.favorites.await(ctx)
					if loadErr == nil {
						sessions, loadErr = prefetch.sessions.await(ctx)
					}
					return loadErr
				}
				listErr = runPrefetchedHomeLoad(command, "Terminal sessions", "Loading sessions", prefetch.favorites.ready() && prefetch.sessions.ready(), work)
				loaded = listErr == nil && prefetch.favorites.fresh() && prefetch.sessions.fresh()
			}
		}
		if !loaded {
			listErr = homeLoading(command, "Terminal sessions", "Loading sessions", func(ctx context.Context) error {
				var err error
				favorites, err = loadFavorites(ctx, client)
				if err != nil {
					return err
				}
				sessions, err = listMachineSessions(ctx, client)
				return err
			})
		}
		if listErr != nil {
			return friendlyCommandError(listErr)
		}
		selected, selectErr := selectMachineSession(command, sessions, favorites)
		if errors.Is(selectErr, errFavoriteToggle) {
			if favoriteErr := setFavorite(command.Context(), client, "session", machineSessionFavoriteID(selected), !favorites.IsFavorite("session", machineSessionFavoriteID(selected))); favoriteErr != nil {
				return favoriteErr
			}
			continue
		}
		if selectErr != nil {
			return selectErr
		}
		if err := actionHomeSessionActions(command, selected.target, selected.session); err != nil && !interactiveCanceled(err) {
			if err := showHomeFailure(command, err); err != nil {
				return err
			}
		}
	}
}

type machineSession struct {
	target  environmentTarget
	session api.TerminalSession
}

func listMachineSessions(ctx context.Context, client *api.Client) ([]machineSession, error) {
	machines, err := client.ListUserMachines(ctx)
	if err != nil {
		return nil, err
	}
	return listMachineSessionsForMachines(ctx, client, machines)
}

// listMachineSessionsForMachines lists the caller's sessions on the given terminal hosts with one
// catalog request (stored rows, no live host probes): one request per machine took tens of seconds
// on accounts with many machines.
func listMachineSessionsForMachines(ctx context.Context, client *api.Client, machines []api.UserMachine) ([]machineSession, error) {
	targets := make(map[string]environmentTarget, len(machines))
	for _, machine := range machines {
		if machine.Capabilities.TerminalHost.Configured {
			targets[machine.ID] = environmentTarget{kind: environmentUserMachine, id: machine.ID, name: machine.Alias}
		}
	}
	if len(targets) == 0 {
		return nil, nil
	}
	catalog, err := client.SharedTerminalSessionsFiltered(ctx, url.Values{"owner": {"mine"}, "live": {"false"}})
	if err != nil {
		return nil, err
	}
	sessions := make([]machineSession, 0, len(catalog))
	for _, item := range catalog {
		target, listed := targets[item.Target.MachineID]
		if !listed {
			continue
		}
		sessions = append(sessions, machineSession{target: target, session: api.TerminalSession{
			ID: item.ID, Name: item.Name, Title: item.Title, Machine: item.Machine, StartedIn: item.StartedIn,
			State: item.State, IsDefault: item.IsDefault, LastActiveAt: item.LastActiveAt,
			CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		}})
	}
	slices.SortStableFunc(sessions, func(a, b machineSession) int {
		return b.activity().Compare(a.activity())
	})
	return sessions, nil
}

func (s machineSession) activity() time.Time {
	if s.session.LastActiveAt != nil {
		return *s.session.LastActiveAt
	}
	if !s.session.UpdatedAt.IsZero() {
		return s.session.UpdatedAt
	}
	return s.session.CreatedAt
}

func selectMachineSession(command *cobra.Command, sessions []machineSession, favorites favoriteSet) (machineSession, error) {
	slices.SortStableFunc(sessions, func(a, b machineSession) int {
		return compareFavorites(favorites.IsFavorite("session", machineSessionFavoriteID(a)), favorites.IsFavorite("session", machineSessionFavoriteID(b)))
	})
	items := make([]selector.Item, 0, len(sessions))
	byID := make(map[string]machineSession, len(sessions))
	for _, entry := range sessions {
		item := machineSessionIdentificationItem(command.Context(), entry, favorites)
		items = append(items, item)
		byID[item.ID] = entry
	}
	client, clientErr := backendForCommand(command)
	if clientErr != nil {
		return machineSession{}, clientErr
	}
	refresh := func(ctx context.Context) ([]selector.Item, error) {
		fresh, err := refreshSessionIdentification(ctx, client, sessions)
		if err != nil {
			return nil, err
		}
		updated := make([]selector.Item, 0, len(fresh))
		for _, entry := range fresh {
			key := entry.target.id + ":" + entry.session.ID
			if _, ok := byID[key]; !ok {
				continue
			}
			updated = append(updated, machineSessionIdentificationItem(command.Context(), entry, favorites))
		}
		return updated, nil
	}
	selected, err := selector.ChooseWithAction(selector.Options{Context: command.Context(), Refresh: refresh, Title: "Terminal sessions", Subtitle: "All machine sessions, newest first", Items: items, Empty: "no terminal sessions are available", Footer: "↑/↓ move  enter open  ctrl+f favorite  esc back", Actions: map[string]string{"ctrl+f": "favorite"}, Stdin: os.Stdin, Output: os.Stderr})
	entry := byID[selected.Item.ID]
	if err == nil && selected.Action == "favorite" {
		err = errFavoriteToggle
	}
	return entry, err
}

func machineSessionFavoriteID(entry machineSession) string {
	return entry.target.id + ":" + entry.session.ID
}

func actionHomeMachines(command *cobra.Command) error {
	client, err := backendForCommand(command)
	if err != nil {
		return err
	}
	usePrefetch := true
	for {
		var favorites favoriteSet
		var machines []api.UserMachine
		var err error
		loaded := false
		if usePrefetch {
			usePrefetch = false
			if prefetch := homePrefetchFor(command); prefetch != nil && prefetch.machinesClaimed.CompareAndSwap(false, true) {
				work := func(ctx context.Context) error {
					var loadErr error
					favorites, loadErr = prefetch.favorites.await(ctx)
					if loadErr == nil {
						machines, loadErr = prefetch.machines.await(ctx)
					}
					return loadErr
				}
				err = runPrefetchedHomeLoad(command, "Machines", "Loading machines", prefetch.favorites.ready() && prefetch.machines.ready(), work)
				loaded = err == nil && prefetch.favorites.fresh() && prefetch.machines.fresh()
			}
		}
		if !loaded {
			err = homeLoading(command, "Machines", "Loading machines", func(ctx context.Context) error {
				var loadErr error
				favorites, loadErr = loadFavorites(ctx, client)
				if loadErr != nil {
					return loadErr
				}
				machines, loadErr = client.ListUserMachines(ctx)
				return loadErr
			})
		}
		if err != nil {
			return friendlyCommandError(err)
		}
		currentMachineID, _ := configuredMachineID()
		sortMachinesForDisplay(machines, favorites, currentMachineID)
		items := make([]selector.Item, 0, len(machines)+1)
		byID := make(map[string]api.UserMachine, len(machines))
		for _, machine := range machines {
			if machine.ID == currentMachineID {
				continue
			}
			favorite := favorites.IsFavorite("machine", machine.ID)
			items = append(items, selector.Item{ID: machine.ID, Title: machine.Alias, Description: preferenceDetails(command.Context(), "machines", map[string]string{"status": machineStatusSummary(machine), "platform": machine.Platform, "id": machine.ID}), Search: machine.WorkspaceRoot + " " + machineStatusSearch(machine) + " favorite starred", Favorite: favorite})
			byID[machine.ID] = machine
		}
		for _, machine := range machines {
			if machine.ID != currentMachineID {
				continue
			}
			favorite := favorites.IsFavorite("machine", machine.ID)
			items = append(items, selector.Item{ID: machine.ID, Title: machineDisplayTitle(machine, currentMachineID), Description: preferenceDetails(command.Context(), "machines", map[string]string{"status": machineStatusSummary(machine), "platform": machine.Platform, "id": machine.ID}), Search: machine.WorkspaceRoot + " " + machineStatusSearch(machine) + " this machine favorite starred", Favorite: favorite})
			byID[machine.ID] = machine
		}
		items = append(items, selector.Item{ID: "add", Title: "+ Add machine", Description: "Set up another computer", Search: "new pair enroll", Action: true})
		selection, selectErr := selector.ChooseWithAction(selector.Options{Title: "Machines", Subtitle: "Paired computers", Items: items, Footer: "↑/↓ move  enter open  ctrl+f favorite  esc back", Actions: map[string]string{"ctrl+f": "favorite"}, Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
		if selectErr != nil {
			return selectErr
		}
		choice := selection.Item
		if choice.ID == "add" && selection.Action == "favorite" {
			continue
		}
		if choice.ID == "add" {
			if addErr := executeInteractiveCommand(command, []string{"machine", "add"}); addErr != nil {
				if interactiveCanceled(addErr) {
					continue
				}
				return addErr
			}
			continue
		}
		machine := byID[choice.ID]
		if selection.Action == "favorite" {
			if favoriteErr := setFavorite(command.Context(), client, "machine", machine.ID, !favorites.IsFavorite("machine", machine.ID)); favoriteErr != nil {
				return favoriteErr
			}
			continue
		}
		for {
			action, actionErr := chooseMachineHomeAction(command, machine)
			if errors.Is(actionErr, selector.ErrCanceled) {
				break
			}
			if actionErr != nil {
				return actionErr
			}
			var runErr error
			switch action.ID {
			case "terminal":
				runErr = executeInteractiveCommand(command, []string{"connect", machine.ID, "new"})
			case "sessions":
				runErr = actionHomeMachineSessions(command, client, machine)
				if interactiveCanceled(runErr) {
					runErr = nil
				}
			case "environment-variables":
				runErr = runEnvironmentVariableScopeTUI(command, client, environmentVariableTarget{machineID: machine.ID, machineName: machine.Alias})
			case "send":
				runErr = actionHomeSendToMachine(command, machine)
			case "rename":
				name, readErr := prompt.Text(prompt.TextOptions{Title: "Rename machine", Description: machine.Alias, Initial: machine.Alias, Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr(), Validate: machinename.Validate})
				if readErr != nil {
					runErr = readErr
					break
				}
				runErr = runHomeResult(command, []string{"machine", "rename", machine.ID, name})
				if runErr == nil {
					machine.Alias = name
				}
			case "allow-sleep":
				runErr = runHomeResult(command, []string{"machine", "availability", machine.ID, "--mode", "allow-sleep"})
			case "keep-awake":
				runErr = runHomeResult(command, []string{"machine", "availability", machine.ID, "--mode", "keep-awake"})
			default:
				runErr = errors.New("unknown machine action")
			}
			if interactiveCanceled(runErr) {
				continue
			}
			if runErr != nil {
				if errors.Is(runErr, selector.ErrInterrupted) {
					return runErr
				}
				if err := showHomeFailure(command, runErr); err != nil {
					return err
				}
			}
		}
	}
}

func machineHomeActions(machine api.UserMachine) []selector.Item {
	actions := make([]selector.Item, 0, 8)
	actions = append(actions, selector.Item{ID: "rename", Title: "Rename", Description: "Change this machine's alias"})
	if machineSupportsEnvironmentInjection(machine) {
		actions = append(actions, selector.Item{ID: "environment-variables", Title: "Environment variables", Description: "Manage variables applied to new processes on this machine"})
	}
	if machine.Capabilities.TerminalHost.Configured {
		actions = append(actions,
			selector.Item{ID: "terminal", Title: "Create terminal session", Description: "Start and attach to a new durable session"},
		)
	}
	if machine.Capabilities.FileReceive.Configured {
		if sourceMachineID, err := configuredMachineID(); err != nil || sourceMachineID != machine.ID {
			actions = append(actions, selector.Item{ID: "send", Title: "Send files", Description: "Search, select, drop, or paste files for this machine"})
		}
	}
	if machine.Capabilities.TerminalHost.Configured {
		actions = append(actions, selector.Item{ID: "sessions", Title: "Sessions", Description: "List durable terminal sessions on this machine"})
	}
	actions = append(actions,
		selector.Item{ID: "allow-sleep", Title: "Allow sleep", Description: "Let normal operating-system sleep policy apply"},
		selector.Item{ID: "keep-awake", Title: "Keep awake", Description: "Request availability even when idle"},
	)
	return actions
}

func chooseMachineHomeAction(command *cobra.Command, machine api.UserMachine) (selector.Item, error) {
	return selector.Choose(selector.Options{Title: machine.Alias, Subtitle: machineStatusSummary(machine), Items: machineHomeActions(machine), Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
}

func machineStatusSummary(machine api.UserMachine) string {
	return machineAvailabilityLabel(machine) + "  ·  " + machineCapabilityLabel(machine)
}

func machineStatusSearch(machine api.UserMachine) string {
	return strings.Join([]string{machineAvailabilityLabel(machine), machineCapabilityLabel(machine)}, " ")
}

func machineAvailabilityLabel(machine api.UserMachine) string {
	if machine.Online {
		return "Online"
	}
	switch strings.ToLower(strings.TrimSpace(machine.State)) {
	case "revoked", "deleted":
		return "Unavailable"
	default:
		return "Offline"
	}
}

func machineCapabilityLabel(machine api.UserMachine) string {
	count := 0
	for _, enabled := range []bool{machine.MachineCapabilities.Desired.Terminal, machine.MachineCapabilities.Desired.ManagedSSH, machine.MachineCapabilities.Desired.FileReceive, machine.MachineCapabilities.Desired.PreviewTunnel} {
		if enabled {
			count++
		}
	}
	return fmt.Sprintf("%d incoming services", count)
}

func machineDisplayTitle(machine api.UserMachine, currentMachineID string) string {
	if machine.ID == currentMachineID {
		return machine.Alias + " (this machine)"
	}
	return machine.Alias
}

func sortMachinesForDisplay(machines []api.UserMachine, favorites favoriteSet, currentMachineID string) {
	slices.SortStableFunc(machines, func(a, b api.UserMachine) int {
		aCurrent, bCurrent := a.ID == currentMachineID, b.ID == currentMachineID
		if aCurrent != bCurrent {
			if aCurrent {
				return 1
			}
			return -1
		}
		return compareFavorites(favorites.IsFavorite("machine", a.ID), favorites.IsFavorite("machine", b.ID))
	})
}

func actionHomeSendToMachine(command *cobra.Command, machine api.UserMachine) error {
	var candidates []string
	searchRoot, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cachePath, err := fileindex.CachePath()
	if err != nil {
		return err
	}
	loadIndex := func(ctx context.Context) error {
		var refreshErr error
		candidates, refreshErr = fileindex.Current(ctx, searchRoot, cachePath)
		return refreshErr
	}
	// A previously written index is sufficient to populate the picker
	// immediately. Refresh it in the background so opening Send Files never
	// blocks on a full profile walk (notably Windows' large AppData tree).
	if cached, cacheOK := fileindex.Load(searchRoot, cachePath); cacheOK {
		candidates = cached
		fileindex.RefreshInBackground(command.Context(), searchRoot, cachePath)
	} else if fileindex.RefreshReady(cachePath) {
		err = loadIndex(command.Context())
	} else {
		err = homeLoading(command, "Send files", "Refreshing files", loadIndex)
	}
	if err != nil {
		return err
	}
	defer fileindex.RefreshInBackground(command.Context(), searchRoot, cachePath)
	items := make([]selector.Item, 0, len(candidates)+1)
	for _, path := range candidates {
		info, statErr := os.Stat(path)
		if statErr != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		display := path
		if relative, relativeErr := filepath.Rel(searchRoot, path); relativeErr == nil {
			display = filepath.Join("~", relative)
		}
		items = append(items, selector.Item{ID: path, Title: display, Description: "File"})
	}
	selection, err := selector.ChooseWithAction(selector.Options{Title: "Send files to " + machine.Alias, Subtitle: "Type to fuzzy-search files in your home folder", Items: items, Empty: "Start typing to search", RequireFilter: true, Footer: "type to search  enter/click send  ctrl+p paste or drop paths  esc back", Actions: map[string]string{"ctrl+p": "paths"}, Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr()})
	if err != nil {
		return err
	}
	if selection.Action != "paths" {
		return executeInteractiveCommand(command, []string{"send", selection.Item.ID, "--to", machine.ID})
	}
	value, err := prompt.Text(prompt.TextOptions{Title: "Send files to " + machine.Alias, Description: "Drag files here or paste one or more file or folder paths", Placeholder: "/path/to/file", Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr(), Validate: func(value string) error {
		_, parseErr := droppedFilePaths(value)
		return parseErr
	}})
	if errors.Is(err, prompt.ErrCanceled) {
		return selector.ErrCanceled
	}
	if err != nil {
		return err
	}
	paths, err := droppedFilePaths(value)
	if err != nil {
		return err
	}
	arguments := append([]string{"send"}, paths...)
	arguments = append(arguments, "--to", machine.ID)
	return executeInteractiveCommand(command, arguments)
}

func droppedFilePaths(value string) ([]string, error) {
	paths, err := splitDroppedPaths(strings.TrimSpace(value))
	if err != nil || len(paths) == 0 {
		return nil, errors.New("drop or paste at least one valid file or folder path")
	}
	for index, path := range paths {
		path = filepath.Clean(path)
		if !filepath.IsAbs(path) {
			absolute, absoluteErr := filepath.Abs(path)
			if absoluteErr != nil {
				return nil, absoluteErr
			}
			path = absolute
		}
		if _, statErr := os.Stat(path); statErr != nil {
			return nil, commandPreparationFailure{step: prepareSelectedFile, cause: statErr}
		}
		paths[index] = path
	}
	return paths, nil
}

func homeLoading(command *cobra.Command, title, detail string, work func(context.Context) error) error {
	return selector.Loading(command.Context(), title, detail, os.Stdin, command.ErrOrStderr(), work)
}

func runPrefetchedHomeLoad(command *cobra.Command, title, detail string, ready bool, work func(context.Context) error) error {
	if ready {
		return work(command.Context())
	}
	return homeLoading(command, title, detail, work)
}

func actionHomeMachineSessions(command *cobra.Command, client *api.Client, machine api.UserMachine) error {
	target := environmentTarget{kind: environmentUserMachine, id: machine.ID, name: machine.Alias}
	for {
		records, err := listTerminalSessionsForTarget(command.Context(), client, target)
		if err != nil {
			return friendlyCommandError(err)
		}
		favorites, err := loadFavorites(command.Context(), client)
		if err != nil {
			return err
		}
		sessions := make([]machineSession, 0, len(records))
		for _, session := range records {
			sessions = append(sessions, machineSession{target: target, session: session})
		}
		selected, err := selectMachineSession(command, sessions, favorites)
		if errors.Is(err, errFavoriteToggle) {
			id := machineSessionFavoriteID(selected)
			if err := setFavorite(command.Context(), client, "session", id, !favorites.IsFavorite("session", id)); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := actionHomeSessionActions(command, target, selected.session); err != nil && !interactiveCanceled(err) {
			if err := showHomeFailure(command, err); err != nil {
				return err
			}
		}
	}
}
