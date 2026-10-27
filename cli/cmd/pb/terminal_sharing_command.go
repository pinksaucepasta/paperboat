package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/pinksaucepasta/paperboat/internal/session"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"github.com/spf13/cobra"
)

const terminalSharingNotice = "Sharing includes up to 64 KiB of recent output, further bounded by attachment capacity, then live output. Recent output may reveal secrets regardless of ENV permissions. Interactive typing may interleave in host receive order and uses the shell's OS permissions."

type terminalSharingFailure struct{ causes []error }

func (terminalSharingFailure) Error() string { return "terminal sharing failed" }

func (e terminalSharingFailure) Unwrap() []error { return e.causes }

func terminalSharingOutputFailure(err error) error {
	if err == nil {
		return nil
	}
	return terminalSharingFailure{causes: []error{err}}
}

func terminalSharingWithCleanup(primary, cleanup error) error {
	if cleanup == nil {
		return primary
	}
	causes := make([]error, 0, 2)
	if primary != nil {
		causes = append(causes, primary)
	}
	causes = append(causes, cleanup)
	return terminalSharingFailure{causes: causes}
}

func addTerminalSharingCommands(root *cobra.Command) {
	for _, action := range []string{"shared", "participants", "share", "remove", "unshare"} {
		root.AddCommand(terminalSharingCommand(action))
	}
	join := &cobra.Command{Use: "join <session-id>", Short: "Join an explicitly shared terminal with recent output", Args: commandArgs(cobra.ExactArgs(1)), RunE: joinSharedTerminal}
	root.AddCommand(join)
}
func terminalSharingCommand(action string) *cobra.Command {
	use := action + " <session-id>"
	nargs := cobra.ExactArgs(1)
	if action == "shared" {
		use = action
		nargs = cobra.NoArgs
	}
	if action == "remove" {
		use += " <account-id>"
		nargs = cobra.ExactArgs(2)
	}
	c := &cobra.Command{Use: use, Short: map[string]string{"shared": "List owned and explicitly shared terminal sessions", "participants": "Show terminal sharing and connected participants", "share": "Grant a team or teammate viewer or interactive access", "remove": "Remove a teammate, including access through an all-team grant", "unshare": "End sharing while preserving the owner's terminal"}[action], Args: commandArgs(nargs)}
	c.Flags().Bool("json", false, "print canonical JSON")
	if action == "shared" {
		inventoryFilterFlags(c)
		c.Flags().Bool("closed", false, "filter explicitly by closed state")
		addListPaging(c)
	}
	if action == "share" {
		c.Long = terminalSharingNotice
		c.Flags().String("team", "", "team ID")
		c.Flags().String("member", "", "selected teammate account ID")
		c.Flags().Bool("all", false, "share with all team members")
		c.Flags().String("role", "viewer", "viewer or interactive")
	}
	c.RunE = func(c *cobra.Command, args []string) error {
		for _, arg := range args {
			if !validTeamCLIIdentifier(arg) {
				return invocationError(errors.New("use an exact session or account ID"))
			}
		}
		in := api.TerminalSharingMutation{}
		if action == "share" {
			in.TeamID, _ = c.Flags().GetString("team")
			in.AccountID, _ = c.Flags().GetString("member")
			in.Role, _ = c.Flags().GetString("role")
			all, _ := c.Flags().GetBool("all")
			if !validTeamCLIIdentifier(in.TeamID) || (all == (in.AccountID != "")) || in.AccountID != "" && !validTeamCLIIdentifier(in.AccountID) || in.Role != "viewer" && in.Role != "interactive" {
				return localArgumentError("share requires --team, exactly one of --all or --member, and --role viewer or interactive")
			}
			in.Audience = "selected_member"
			if all {
				in.Audience = "all_members"
			}
			in.Active = true
		}
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		jsonOutput, _ := c.Flags().GetBool("json")
		if action == "shared" {
			maxItems, err := listPagingMax(c)
			if err != nil {
				return err
			}
			sessions, total, err := sharedSessionsEverywhere(c.Context(), client, inventoryFilters(c), maxItems)
			if err != nil {
				return err
			}
			// The JSON stays a list; the footer on stderr says when it is not the whole list.
			defer writeListFooter(c.ErrOrStderr(), len(sessions), total, "sessions")
			if jsonOutput {
				return terminalSharingOutputFailure(json.NewEncoder(c.OutOrStdout()).Encode(sessions))
			}
			if len(sessions) == 0 {
				_, err := fmt.Fprintln(c.ErrOrStderr(), "No sessions are shared with you.")
				return terminalSharingOutputFailure(err)
			}
			for _, s := range sessions {
				if _, err := fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.ID, selector.SanitizeText(s.Name), sessionActivityLabel(api.TerminalSession{Title: s.Title, ForegroundProcess: s.ForegroundProcess}), sessionDirectoryLabel(api.TerminalSession{CurrentDirectory: s.CurrentDirectory, StartedIn: s.StartedIn}), s.Target.Name, s.Role, s.Sharing.Audience); err != nil {
					return terminalSharingOutputFailure(err)
				}
			}
			return nil
		}
		state, err := client.TerminalSharing(c.Context(), args[0])
		if err != nil {
			return err
		}
		if action != "participants" {
			if !state.CanManage {
				return errors.New("only the session owner can change sharing")
			}
			in.OperationID = newIdempotencyKey()
			in.ExpectedGeneration = state.Sharing.Generation
			if action == "share" && state.Sharing.TeamID == "" {
				team, teamErr := client.GetTeam(c.Context(), in.TeamID)
				if teamErr != nil {
					return teamErr
				}
				in.ExpectedGeneration = team.Generation
			}
			switch action {
			case "share":
				if _, err := fmt.Fprintln(c.ErrOrStderr(), terminalSharingNotice); err != nil {
					return terminalSharingOutputFailure(err)
				}
				state, err = client.GrantTerminalSharing(c.Context(), args[0], in)
			case "remove":
				state, err = client.RemoveTerminalParticipant(c.Context(), args[0], args[1], in)
			case "unshare":
				state, err = client.EndTerminalSharing(c.Context(), args[0], in)
			}
			if err != nil {
				return err
			}
		}
		if jsonOutput {
			return terminalSharingOutputFailure(json.NewEncoder(c.OutOrStdout()).Encode(state))
		}
		if _, err := fmt.Fprintf(c.OutOrStdout(), "%s (%s) · %s · sharing: %s\n", state.Name, state.ID, state.Role, state.Sharing.Audience); err != nil {
			return terminalSharingOutputFailure(err)
		}
		if !state.ParticipantsAvailable {
			_, err := fmt.Fprintln(c.OutOrStdout(), "Live participants unavailable; the runtime may be offline.")
			return terminalSharingOutputFailure(err)
		}
		for _, p := range state.Participants {
			if _, err := fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s\t%s\n", p.AccountID, p.Role, p.ClientID, p.AttachmentID); err != nil {
				return terminalSharingOutputFailure(err)
			}
		}
		return nil
	}
	return c
}

type sharedInputSink struct{ io.Writer }

func (sharedInputSink) Close() error { return nil }

func joinSharedTerminal(c *cobra.Command, args []string) (result error) {
	id := strings.TrimSpace(args[0])
	if !validTeamCLIIdentifier(id) {
		return invocationError(errors.New("use an exact shared session ID"))
	}
	d, err := buildDeps(actionContext(c, nil))
	if err != nil {
		return err
	}
	source, err := configuredMachineID()
	if err != nil {
		return err
	}
	lookup, err := newWorkspaceAPIClient(actionContext(c, nil), d.cfg.ServerURL, d.auth)
	if err != nil {
		return err
	}
	workspace, err := sharedSessionWorkspace(c.Context(), lookup, id)
	if err != nil {
		return err
	}
	var cursor atomic.Int64
	attachment := "attachment_" + uuid.NewString()
	inputQueue := resolver.NewTerminalInputQueue(256)
	resolve := func(ctx context.Context) (resolver.ConnectInfo, error) {
		_, err = d.auth.Credential()
		if err != nil {
			return resolver.ConnectInfo{}, err
		}
		client, err := newWorkspaceAPIClient(actionContext(c, nil), d.cfg.ServerURL, d.auth)
		if err != nil {
			return resolver.ConnectInfo{}, err
		}
		if err := client.SetWorkspace(workspace); err != nil {
			return resolver.ConnectInfo{}, err
		}
		client.SetSourceMachineID(source)
		r := resolver.NewAPIResolver(client, d.cfg)
		r.Progress = func(status, reason string, retryAfter time.Duration) {
			fmt.Fprintf(c.ErrOrStderr(), "Connecting: %s (%s), retrying in %s...\n", status, reason, retryAfter.Round(time.Second))
		}
		info, err := r.ResolveShared(ctx, id)
		if err != nil {
			return info, err
		}
		info.Terminal.AfterSequence = int(cursor.Load())
		info.Terminal.InputAttachmentID = attachment
		info.Terminal.InputQueue = inputQueue
		info.Terminal.SequenceSink = func(n int) {
			for {
				old := cursor.Load()
				if int64(n) <= old || cursor.CompareAndSwap(old, int64(n)) {
					return
				}
			}
		}
		return info, nil
	}
	info, err := resolve(c.Context())
	if err != nil {
		return err
	}
	initial, err := dialSharedTerminal(c.Context(), c.ErrOrStderr(), d.tunnel.Dial, info, id)
	if err != nil {
		return err
	}
	var viewOnly atomic.Bool
	viewOnly.Store(info.Terminal.ViewOnly())
	conn := tunnel.NewReconnectingConn(c.Context(), initial, d.cfg.Connect.DialRetries, time.Duration(d.cfg.Connect.DialRetrySeconds)*time.Second, func(ctx context.Context) (tunnel.Conn, error) {
		fresh, err := resolve(ctx)
		if err != nil {
			var apiErr *api.APIError
			if errors.As(err, &apiErr) && (apiErr.Status == 403 || apiErr.Status == 404) {
				return nil, tunnel.StopReconnect(err)
			}
			return nil, err
		}
		next, err := d.tunnel.Dial(ctx, fresh)
		if err == nil && viewOnly.Swap(fresh.Terminal.ViewOnly()) != fresh.Terminal.ViewOnly() {
			fmt.Fprintln(c.ErrOrStderr(), sharedRoleNotice(fresh.Terminal.ViewOnly()))
		}
		return next, err
	})
	if client, err := lookup.ForWorkspace(workspace); err == nil {
		go watchSharedRole(c.Context(), sharedRoleInterval, func(ctx context.Context) (bool, error) {
			state, err := client.TerminalSharing(ctx, id)
			return state.Role == "viewer", err
		}, viewOnly.Load, conn.Redial)
	}
	defer func() { result = terminalSharingWithCleanup(result, conn.Close()) }()
	opts := []session.RunOption{session.WithOutput(c.OutOrStdout()), session.WithOutputBufferBytes(d.cfg.Connect.TerminalOutputBufferBytes)}
	opts = append(opts, session.WithReadOnlyWhile(viewOnly.Load))
	code, err := session.Run(c.Context(), conn, sharedInputSink{conn}, opts...)
	if err != nil {
		return err
	}
	if code != 0 {
		return exitCodeError{code: code}
	}
	return nil
}

// dialSharedTerminal connects first and only then says the terminal is joined: a join that is still
// connecting, or fails, must not print a banner that looks like an attached terminal.
func dialSharedTerminal(ctx context.Context, w io.Writer, dial func(context.Context, resolver.ConnectInfo) (tunnel.Conn, error), info resolver.ConnectInfo, id string) (tunnel.Conn, error) {
	if _, err := fmt.Fprintf(w, "Connecting to shared terminal %s...\n", id); err != nil {
		return nil, terminalSharingOutputFailure(err)
	}
	conn, err := dial(ctx, info)
	if err != nil {
		return nil, err
	}
	role := "interactive"
	if info.Terminal.ViewOnly() {
		role = "view only; Ctrl-C detaches"
	}
	if _, err := fmt.Fprintf(w, "%s\nShared terminal %s · %s. Use `pb session participants %s` to see participants.\n", terminalSharingNotice, id, role, id); err != nil {
		_ = conn.Close()
		return nil, terminalSharingOutputFailure(err)
	}
	return conn, nil
}

// sharedRoleInterval bounds how long a joined teammate waits for a role the owner changed.
const sharedRoleInterval = 5 * time.Second

// watchSharedRole redials the joined terminal when the member's role on it changes, so an owner's
// upgrade to interactive (or downgrade to viewer) takes effect without a rejoin.
func watchSharedRole(ctx context.Context, interval time.Duration, currentlyViewer func(context.Context) (bool, error), joinedAsViewer func() bool, redial func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		viewer, err := currentlyViewer(ctx)
		if err == nil && viewer != joinedAsViewer() {
			redial()
		}
	}
}

func sharedRoleNotice(viewOnly bool) string {
	if viewOnly {
		return "Your role on this shared terminal is now view only; typing stays local and Ctrl-C detaches."
	}
	return "Your role on this shared terminal is now interactive; typing reaches the shared shell."
}
