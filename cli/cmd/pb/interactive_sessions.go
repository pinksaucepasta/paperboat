package main

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/prompt"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/spf13/cobra"
	"net/url"
	"os"
	"strings"
)

func actionHomeSessions(command *cobra.Command) error {
	for {
		choice, err := chooseHomeAction(command, "Terminal sessions", []selector.Item{
			{ID: "owned", Title: "Machine sessions", Description: "Attach, rename, share, close or delete sessions"},
			{ID: "shared", Title: "Shared sessions", Description: "Discover and join explicitly shared sessions"},
		})
		if err != nil {
			return err
		}
		if choice.ID == "owned" {
			err = actionHomeOwnedSessions(command)
		} else {
			err = actionHomeSharedSessions(command)
		}
		if err != nil && !interactiveCanceled(err) {
			if err := showHomeFailure(command, err); err != nil {
				return err
			}
		}
	}
}

func actionHomeSessionActions(command *cobra.Command, target environmentTarget, session api.TerminalSession) error {
	for {
		actions := homeSessionActions(session)
		action, actionErr := chooseHomeAction(command, session.Name, actions)
		if errors.Is(actionErr, selector.ErrCanceled) {
			return nil
		}
		if actionErr != nil {
			return actionErr
		}
		if action.ID == "sharing" {
			if err := actionHomeSessionSharing(command, session.ID); err != nil && !interactiveCanceled(err) {
				return err
			}
			continue
		}
		if action.ID == "attach" {
			if attachErr := executeInteractiveCommand(command, []string{"session", "attach", target.id, session.ID}); attachErr != nil {
				return attachErr
			}
			return nil
		}
		if action.ID == "rename" {
			name, promptErr := prompt.Text(prompt.TextOptions{Title: "Rename session", Description: target.name + "  ·  " + session.Name, Placeholder: session.Name, Stdin: os.Stdin, Context: command.Context(), Output: command.ErrOrStderr(), Validate: func(value string) error {
				if strings.TrimSpace(value) == "" {
					return errors.New("session name is required")
				}
				return nil
			}})
			if errors.Is(promptErr, prompt.ErrCanceled) {
				continue
			}
			if promptErr != nil {
				return promptErr
			}
			if mutationErr := runHomeResult(command, []string{"session", "rename", target.id, session.ID, name}); mutationErr != nil {
				return mutationErr
			}
			return nil
		}
		if mutationErr := runHomeResult(command, []string{"session", action.ID, target.id, session.ID}); mutationErr != nil {
			return mutationErr
		}
		return nil
	}
}

func actionHomeSharedSessions(command *cobra.Command) error {
	client, err := backendForCommand(command)
	if err != nil {
		return err
	}
	for {
		sessions, err := client.SharedTerminalSessionsFiltered(command.Context(), url.Values{"owner": {"shared"}})
		if err != nil {
			return err
		}
		items := []selector.Item{}
		for _, session := range sessions {
			items = append(items, selector.Item{ID: session.ID, Title: session.Name, Description: session.Target.Name + " · " + session.Role + " · " + session.Sharing.Audience})
		}
		items = append(items, selector.Item{ID: "refresh", Title: "Refresh", Action: true})
		choice, err := chooseHomeAction(command, "Shared sessions", items)
		if err != nil {
			return err
		}
		if choice.ID == "refresh" {
			continue
		}
		action, err := chooseHomeAction(command, choice.Title, []selector.Item{{ID: "join", Title: "Join session", Description: terminalSharingNotice}, {ID: "sharing", Title: "Sharing & participants"}})
		if interactiveCanceled(err) {
			continue
		}
		if err != nil {
			return err
		}
		if action.ID == "sharing" {
			err = actionHomeSessionSharing(command, choice.ID)
		} else {
			err = executeInteractiveCommand(command, []string{"session", "join", choice.ID})
		}
		if err != nil && !interactiveCanceled(err) {
			if err := showHomeFailure(command, err); err != nil {
				return err
			}
		}
	}
}

func actionHomeSessionSharing(command *cobra.Command, sessionID string) error {
	client, err := backendForCommand(command)
	if err != nil {
		return err
	}
	for {
		state, err := client.TerminalSharing(command.Context(), sessionID)
		if err != nil {
			return err
		}
		items := []selector.Item{{ID: "participants", Title: "Current sharing & participants"}}
		if state.CanManage {
			items = append(items, selector.Item{ID: "share", Title: "Share with teammates", Description: terminalSharingNotice}, selector.Item{ID: "remove", Title: "Remove teammate access"}, selector.Item{ID: "unshare", Title: "End sharing"})
		}
		action, err := chooseHomeAction(command, state.Name, items)
		if err != nil {
			return err
		}
		args := []string{"session", action.ID, sessionID}
		if action.ID == "share" {
			teams, err := client.ListTeams(command.Context())
			if err != nil {
				return err
			}
			choices := []selector.Item{}
			for _, team := range teams {
				if client.Workspace() == "" || client.Workspace() == "personal" || client.Workspace() == team.TeamID {
					choices = append(choices, selector.Item{ID: team.TeamID, Title: team.TeamID})
				}
			}
			team, err := chooseHomeAction(command, "Choose team", choices)
			if interactiveCanceled(err) {
				continue
			}
			if err != nil {
				return err
			}
			record, err := client.GetTeam(command.Context(), team.ID)
			if err != nil {
				return err
			}
			choices = []selector.Item{{ID: "all", Title: "All team members"}}
			for _, member := range record.Members {
				if member.Active {
					choices = append(choices, selector.Item{ID: member.AccountID, Title: member.AccountID})
				}
			}
			audience, err := chooseHomeAction(command, "Choose audience", choices)
			if interactiveCanceled(err) {
				continue
			}
			if err != nil {
				return err
			}
			role, err := chooseValue(command, "Terminal access", "viewer", []string{"viewer", "interactive"})
			if interactiveCanceled(err) {
				continue
			}
			if err != nil {
				return err
			}
			yes, err := prompt.Confirm(prompt.ConfirmOptions{Context: command.Context(), Title: "Share this terminal?", Description: terminalSharingNotice, Stdin: os.Stdin, Output: command.ErrOrStderr()})
			if interactiveCanceled(err) {
				continue
			}
			if err != nil {
				return err
			}
			if !yes {
				continue
			}
			args = append(args, "--team", team.ID, "--role", role)
			if audience.ID == "all" {
				args = append(args, "--all")
			} else {
				args = append(args, "--member", audience.ID)
			}
		}
		if action.ID == "remove" {
			account, err := prompt.Text(prompt.TextOptions{Context: command.Context(), Title: "Teammate account ID", Stdin: os.Stdin, Output: command.ErrOrStderr(), Validate: func(s string) error {
				if !validTeamCLIIdentifier(s) {
					return errors.New("use an exact account ID")
				}
				return nil
			}})
			if interactiveCanceled(err) {
				continue
			}
			if err != nil {
				return err
			}
			args = append(args, account)
		}
		if err := runHomeResult(command, args); err != nil {
			return err
		}
	}
}

func homeSessionActions(session api.TerminalSession) []selector.Item {
	actions := []selector.Item{}
	if session.State != "closed" {
		actions = append(actions, selector.Item{ID: "attach", Title: "Attach", Description: "Open this durable terminal session"}, selector.Item{ID: "sharing", Title: "Sharing & participants", Description: "Grant, inspect or end explicit teammate access"})
	}
	actions = append(actions, selector.Item{ID: "rename", Title: "Rename", Description: "Change the name shown in the session catalog"})
	if session.State != "closed" {
		actions = append(actions, selector.Item{ID: "close", Title: "Close", Description: "Stop its process and delete recent output"})
	} else if !session.IsDefault {
		actions = append(actions, selector.Item{ID: "delete", Title: "Delete", Description: "Permanently remove this closed terminal record"})
	}
	return actions
}
