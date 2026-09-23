package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/session"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"github.com/spf13/cobra"
)

const terminalSharingNotice = "Sharing includes up to 64 KiB of recent output, further bounded by attachment capacity, then live output. Recent output may reveal secrets regardless of ENV permissions. Interactive typing may interleave in host receive order and uses the shell's OS permissions."

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
				return invocationError(errors.New("share requires --team, exactly one of --all or --member, and --role viewer or interactive"))
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
			sessions, err := client.SharedTerminalSessions(c.Context())
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(c.OutOrStdout()).Encode(sessions)
			}
			for _, s := range sessions {
				fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\n", s.ID, s.Name, s.Target.Name, s.Role, s.Sharing.Audience)
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
				fmt.Fprintln(c.ErrOrStderr(), terminalSharingNotice)
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
			return json.NewEncoder(c.OutOrStdout()).Encode(state)
		}
		fmt.Fprintf(c.OutOrStdout(), "%s (%s) · %s · sharing: %s\n", state.Name, state.ID, state.Role, state.Sharing.Audience)
		if !state.ParticipantsAvailable {
			fmt.Fprintln(c.OutOrStdout(), "Live participants unavailable; the runtime may be offline.")
			return nil
		}
		for _, p := range state.Participants {
			fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s\t%s\n", p.AccountID, p.Role, p.ClientID, p.AttachmentID)
		}
		return nil
	}
	return c
}

type sharedInputSink struct{ io.Writer }

func (sharedInputSink) Close() error { return nil }

func joinSharedTerminal(c *cobra.Command, args []string) error {
	id := strings.TrimSpace(args[0])
	if !validTeamCLIIdentifier(id) {
		return invocationError(errors.New("use an exact shared session ID"))
	}
	d, err := buildDeps(actionContext(c, nil))
	if err != nil {
		return err
	}
	if d.hostedTransferKeys != nil {
		defer d.hostedTransferKeys.Close()
	}
	source, err := configuredMachineID()
	if err != nil {
		return err
	}
	var cursor atomic.Int64
	attachment := "att_" + newIdempotencyKey()
	inputQueue := resolver.NewTerminalInputQueue(256)
	resolve := func(ctx context.Context) (resolver.ConnectInfo, error) {
		credential, err := d.auth.Credential()
		if err != nil {
			return resolver.ConnectInfo{}, err
		}
		client := api.New(d.cfg.ServerURL, credential, nil)
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
	fmt.Fprintln(c.ErrOrStderr(), terminalSharingNotice)
	role := "interactive"
	if info.Terminal.ViewOnly() {
		role = "view only; Ctrl-C detaches"
	}
	fmt.Fprintf(c.ErrOrStderr(), "Shared terminal %s · %s. Use `pb session participants %s` to see participants.\n", id, role, id)
	initial, err := d.tunnel.Dial(c.Context(), info)
	if err != nil {
		return err
	}
	conn := tunnel.NewReconnectingConn(c.Context(), initial, d.cfg.Connect.DialRetries, time.Duration(d.cfg.Connect.DialRetrySeconds)*time.Second, func(ctx context.Context) (tunnel.Conn, error) {
		fresh, err := resolve(ctx)
		if err != nil {
			var apiErr *api.APIError
			if errors.As(err, &apiErr) && (apiErr.Status == 403 || apiErr.Status == 404) {
				return nil, tunnel.StopReconnect(err)
			}
			return nil, err
		}
		if fresh.Terminal.ViewOnly() != info.Terminal.ViewOnly() {
			return nil, tunnel.StopReconnect(errors.New("your terminal role changed; run `pb session join " + id + "` again to use the current role"))
		}
		return d.tunnel.Dial(ctx, fresh)
	})
	defer conn.Close()
	opts := []session.RunOption{session.WithOutput(c.OutOrStdout()), session.WithOutputBufferBytes(d.cfg.Connect.TerminalOutputBufferBytes)}
	if info.Terminal.ViewOnly() {
		opts = append(opts, session.WithReadOnly())
	}
	code, err := session.Run(c.Context(), conn, sharedInputSink{conn}, opts...)
	if err != nil {
		return err
	}
	if code != 0 {
		return exitCodeError{code: code}
	}
	return nil
}
