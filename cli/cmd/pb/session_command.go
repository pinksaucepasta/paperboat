package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// sessionCobraCommand declares `pb session` and its subcommands.
func sessionCobraCommand() *cobra.Command {
	group := &cobra.Command{Use: "session", Short: "Manage environment terminal sessions", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error {
		if term.IsTerminal(int(os.Stdin.Fd())) {
			return actionHomeSessions(command)
		}
		return command.Help()
	}}
	attach := &cobra.Command{Use: "attach [environment] [session]", Short: "Choose and attach to a durable terminal session", Args: commandArgs(cobra.MaximumNArgs(2)), RunE: func(cobraCommand *cobra.Command, args []string) error {
		if err := validateConnectInvocation(cobraCommand); err != nil {
			return err
		}
		ctx := actionContext(cobraCommand, args)
		d, err := buildDeps(ctx)
		if err != nil {
			return err
		}
		client, err := newWorkspaceAPIClient(ctx, d.cfg.ServerURL, d.auth)
		if err != nil {
			return err
		}
		environmentRef := ""
		sessionRef := ""
		switch len(args) {
		case 2:
			environmentRef, sessionRef = args[0], args[1]
		case 1:
			sessionRef = args[0]
			environmentRef, err = defaultWorkspaceEnvironment(ctx.Context, ctx, client, d.cfg)
		default:
			environmentRef, err = selectEnvironment(ctx.Context, client, "Choose an environment")
		}
		if err != nil {
			return err
		}
		target, err := resolveTerminalEnvironmentTarget(ctx.Context, client, environmentRef)
		if err != nil {
			return err
		}
		if sessionRef == "" {
			sessions, listErr := listTerminalSessionsForTarget(ctx.Context, client, target)
			if listErr != nil {
				return friendlyCommandError(listErr)
			}
			selected, selectErr := selectSession(ctx.Context, client, target, sessions, "Choose a terminal session")
			if selectErr != nil {
				return selectErr
			}
			sessionRef = selected.ID
		}
		if err := cobraCommand.Flags().Set("session", sessionRef); err != nil {
			return err
		}
		return actionConnectTarget(actionContext(cobraCommand, nil), environmentRef)
	}}
	attach.Flags().String("session", "", "")
	_ = attach.Flags().MarkHidden("session")
	attach.Flags().Bool("debug", false, "show the pb versions used by this terminal session")
	addStatusBarFlags(attach)
	group.AddCommand(attach)
	list := &cobra.Command{Use: "list [environment]", Short: "List durable terminal sessions", Args: commandArgs(cobra.MaximumNArgs(1)), RunE: actionRun(sessionList)}
	list.Flags().Bool("wide", false, "include immutable IDs")
	list.Flags().Bool("json", false, "print JSON")
	inventoryFilterFlags(list)
	list.Flags().Bool("closed", false, "filter explicitly by closed state")
	group.AddCommand(list)
	for _, child := range []struct {
		name, short string
		args        cobra.PositionalArgs
		run         command.Action
	}{
		{"rename", "Rename a terminal session", cobra.ExactArgs(3), sessionRename},
		{"close", "Close one or all terminal sessions", cobra.RangeArgs(1, 2), sessionClose},
		{"delete", "Delete a closed terminal session record", cobra.RangeArgs(1, 2), sessionDelete},
	} {
		entry := &cobra.Command{Use: child.name, Short: child.short, Args: commandArgs(child.args), RunE: actionRun(child.run)}
		if child.name == "rename" {
			entry.Flags().Bool("json", false, "print JSON")
		} else {
			entry.Flags().String("confirm", "", "six-character confirmation code from the preview")
			entry.Flags().Bool("json", false, "print JSON")
			entry.Flags().Bool("all", false, child.name+" all sessions in the environment")
		}
		group.AddCommand(entry)
	}
	addTerminalSharingCommands(group)
	return group
}

func sessionList(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	requested := strings.TrimSpace(c.Args().First())
	if requested == "" {
		cfg, loadErr := workspaceConfig(c.String("config"), c.String("server"))
		if loadErr != nil {
			return loadErr
		}
		requested, err = defaultWorkspaceEnvironment(c.Context, c, client, cfg)
		if err != nil {
			return err
		}
	}
	target, err := resolveTerminalEnvironmentTarget(c.Context, client, requested)
	if err != nil {
		return err
	}
	sessions, err := client.ListUserMachineTerminalSessionsFiltered(c.Context, target.id, commandInventoryFilters(c))
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "environment": map[string]string{"id": target.id, "kind": target.kind, "alias": target.name}, "sessions": sessions})
	}
	w := tabwriter.NewWriter(c.Writer, 0, 4, 2, ' ', 0)
	if c.Bool("wide") {
		fmt.Fprintln(w, "NAME\tID\tTITLE / PROCESS\tDIRECTORY\tSTATE\tATTACHED\tLAST ACTIVE\tCREATED")
	} else {
		fmt.Fprintln(w, "NAME\tTITLE / PROCESS\tDIRECTORY\tSTATE\tATTACHED\tLAST ACTIVE\tCREATED")
	}
	for _, s := range sessions {
		attached := "-"
		if s.AttachedCount != nil {
			attached = fmt.Sprintf("%d", *s.AttachedCount)
		}
		if c.Bool("wide") {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", selector.SanitizeText(s.Name), s.ID, sessionActivityLabel(s), sessionDirectoryLabel(s), s.State, attached, relativeTime(s.LastActiveAt), relativeTimestamp(s.CreatedAt))
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", selector.SanitizeText(s.Name), sessionActivityLabel(s), sessionDirectoryLabel(s), s.State, attached, relativeTime(s.LastActiveAt), relativeTimestamp(s.CreatedAt))
		}
	}
	return w.Flush()
}

func sessionRename(c *command.Context) error {
	if c.Args().Len() != 3 {
		return localArgumentError("usage: pb session rename <environment> <session> <new-name>")
	}
	if err := validateSessionName(c.Args().Get(2)); err != nil {
		return err
	}
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	target, err := resolveTerminalEnvironmentTarget(c.Context, client, c.Args().First())
	if err != nil {
		return err
	}
	session, err := resolveTerminalSession(c.Context, client, target, c.Args().Get(1))
	if err != nil {
		return err
	}
	if session.IsDefault {
		return commandRejection{reason: commandRejectDefaultSessionRename}
	}
	updated, err := renameTerminalSessionForTarget(c.Context, client, target, session.ID, c.Args().Get(2))
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return writeCLIJSON(c.Writer, map[string]any{"environment": map[string]string{"id": target.id, "kind": target.kind, "alias": target.name}, "session": updated})
	}
	fmt.Fprintf(c.Writer, "Renamed session %s to %s.\n", session.Name, updated.Name)
	return nil
}

func sessionClose(c *command.Context) error {
	all := c.Bool("all")
	if c.Args().Len() < 1 || c.Args().Len() > 2 || all && c.Args().Len() != 1 {
		return localArgumentError("usage: pb session close <environment> [<session>] [--all] [--confirm CODE]")
	}
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	target, err := resolveTerminalEnvironmentTarget(c.Context, client, c.Args().First())
	if err != nil {
		return err
	}
	if all {
		sessions, err := listTerminalSessionsForTarget(c.Context, client, target)
		if err != nil {
			return friendlyCommandError(err)
		}
		open := make([]api.TerminalSession, 0, len(sessions))
		for _, session := range sessions {
			if session.State != "closed" {
				open = append(open, session)
			}
		}
		if !c.Bool("json") {
			fmt.Fprintf(c.ErrWriter, "Environment: %s (%s)\n", target.name, target.id)
			fmt.Fprintf(c.ErrWriter, "Open sessions to close: %d\n", len(open))
		}
		ids := make([]string, 0, len(open))
		for _, session := range open {
			ids = append(ids, session.ID)
		}
		if len(open) == 0 {
			if c.Bool("json") {
				return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "environment": map[string]string{"id": target.id, "kind": target.kind, "alias": target.name}, "closed": 0, "pending": 0})
			}
			fmt.Fprintln(c.Writer, "No open sessions to close.")
			return nil
		}
		if err := confirmContextMutation(c, "session-close-all:"+target.id+":"+sortedSessionScope(ids), fmt.Sprintf("Close %d open sessions in %s (%s)? Their remote processes will end and recent output will be deleted.", len(open), target.name, target.id)); err != nil {
			return err
		}
		var closeErrors []error
		closed, pending := 0, 0
		for _, session := range open {
			result, err := closeTerminalSessionForTarget(c.Context, client, target, session.ID)
			if err != nil {
				closeErrors = append(closeErrors, fmt.Errorf("close session %s: %w", session.Name, err))
				continue
			}
			if result.OperationState == "applied" {
				closed++
			} else {
				pending++
			}
		}
		if len(closeErrors) > 0 {
			return fmt.Errorf("closed %d sessions, with %d close requests pending in %s; rerun without --confirm to inspect remaining sessions: %w", closed, pending, target.name, errors.Join(closeErrors...))
		}
		if c.Bool("json") {
			return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "environment": map[string]string{"id": target.id, "kind": target.kind, "alias": target.name}, "closed": closed, "pending": pending})
		}
		fmt.Fprintf(c.Writer, "Closed %d sessions in %s. Recent output was deleted.\n", closed, target.name)
		if pending > 0 {
			fmt.Fprintf(c.Writer, "%d close requests are pending host confirmation. Check session list before deleting their records.\n", pending)
		}
		return nil
	}
	var session api.TerminalSession
	if c.Args().Len() == 1 {
		sessions, listErr := listTerminalSessionsForTarget(c.Context, client, target)
		if listErr != nil {
			return friendlyCommandError(listErr)
		}
		session, err = selectSession(c.Context, client, target, slices.DeleteFunc(sessions, func(item api.TerminalSession) bool { return item.State == "closed" }), "Choose a session to close")
	} else {
		session, err = resolveTerminalSession(c.Context, client, target, c.Args().Get(1))
	}
	if err != nil {
		return err
	}
	if err := confirmContextMutationWithArgs(c, "session-close:"+target.id+":"+session.ID, fmt.Sprintf("Close terminal session %q in %s (%s)? Its process will end and recent output will be deleted.", session.Name, target.name, target.id), []string{c.Args().First(), session.ID}); err != nil {
		return err
	}
	result, err := closeTerminalSessionForTarget(c.Context, client, target, session.ID)
	if err != nil {
		return friendlyCommandError(err)
	}
	state := "closing"
	if result.OperationState == "applied" {
		state = "closed"
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "environment": map[string]string{"id": target.id, "kind": target.kind, "alias": target.name}, "session_id": session.ID, "state": state, "operation_state": result.OperationState})
	}
	if result.OperationState == "pending" {
		fmt.Fprintln(c.Writer, "Close requested; waiting for host confirmation. Check session list before deleting its record.")
	} else {
		fmt.Fprintln(c.Writer, "Session closed. Recent output was deleted.")
	}
	return nil
}

func sessionDelete(c *command.Context) error {
	all := c.Bool("all")
	if c.Args().Len() < 1 || c.Args().Len() > 2 || all && c.Args().Len() != 1 {
		return localArgumentError("usage: pb session delete <environment> [<session>] [--all] [--confirm CODE]")
	}
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	target, err := resolveTerminalEnvironmentTarget(c.Context, client, c.Args().First())
	if err != nil {
		return err
	}
	if all {
		sessions, err := listTerminalSessionsForTarget(c.Context, client, target)
		if err != nil {
			return friendlyCommandError(err)
		}
		selected := slices.DeleteFunc(sessions, func(item api.TerminalSession) bool { return item.IsDefault || item.State != "closed" })
		if !c.Bool("json") {
			fmt.Fprintf(c.ErrWriter, "Environment: %s (%s)\n", target.name, target.id)
			fmt.Fprintf(c.ErrWriter, "Closed non-default sessions to delete: %d\n", len(selected))
		}
		ids := make([]string, 0, len(selected))
		for _, session := range selected {
			ids = append(ids, session.ID)
		}
		if len(selected) == 0 {
			if c.Bool("json") {
				return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "environment": map[string]string{"id": target.id, "kind": target.kind, "alias": target.name}, "deleted": 0})
			}
			fmt.Fprintln(c.Writer, "No closed non-default sessions to delete.")
			return nil
		}
		if err := confirmContextMutation(c, "session-delete-all:"+target.id+":"+sortedSessionScope(ids), fmt.Sprintf("Delete %d closed non-default sessions in %s (%s)? Their records and retained history will be removed. Open and default sessions are excluded.", len(selected), target.name, target.id)); err != nil {
			return err
		}
		var deleteErrors []error
		deleted := 0
		for _, session := range selected {
			if err := deleteTerminalSessionForTarget(c.Context, client, target, session.ID); err != nil {
				deleteErrors = append(deleteErrors, fmt.Errorf("delete session %s: %w", session.Name, err))
				continue
			}
			deleted++
		}
		if len(deleteErrors) > 0 {
			return fmt.Errorf("deleted %d of %d sessions in %s; remote state changed; rerun without --confirm to preview the remaining sessions: %w", deleted, len(selected), target.name, errors.Join(deleteErrors...))
		}
		if c.Bool("json") {
			return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "environment": map[string]string{"id": target.id, "kind": target.kind, "alias": target.name}, "deleted": deleted})
		}
		fmt.Fprintf(c.Writer, "Deleted %d closed session records in %s.\n", deleted, target.name)
		return nil
	}
	var session api.TerminalSession
	if c.Args().Len() == 1 {
		sessions, listErr := listTerminalSessionsForTarget(c.Context, client, target)
		if listErr != nil {
			return friendlyCommandError(listErr)
		}
		session, err = selectSession(c.Context, client, target, slices.DeleteFunc(sessions, func(item api.TerminalSession) bool { return item.IsDefault || item.State != "closed" }), "Choose a closed session to delete")
	} else {
		session, err = resolveTerminalSession(c.Context, client, target, c.Args().Get(1))
	}
	if err != nil {
		return err
	}
	if session.IsDefault {
		return commandRejection{reason: commandRejectDefaultSessionDelete}
	}
	if session.State != "closed" {
		return commandRejection{reason: commandRejectOpenSessionDelete}
	}
	if err := confirmContextMutationWithArgs(c, "session-delete:"+target.id+":"+session.ID, fmt.Sprintf("Delete closed terminal session %q in %s (%s)? Its record and retained history will be removed.", session.Name, target.name, target.id), []string{c.Args().First(), session.ID}); err != nil {
		return err
	}
	if err := deleteTerminalSessionForTarget(c.Context, client, target, session.ID); err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"version": "1", "environment": map[string]string{"id": target.id, "kind": target.kind, "alias": target.name}, "session_id": session.ID, "deleted": true})
	}
	return nil
}
