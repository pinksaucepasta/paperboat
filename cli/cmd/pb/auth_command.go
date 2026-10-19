package main

import (
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// authLoginCommand declares the browser sign-in command. It is registered twice:
// as `pb auth login` and as the top-level `pb login`.
func authLoginCommand() *cobra.Command {
	login := &cobra.Command{Use: "login", Short: "Add this computer to your account, or sign in again, through browser approval", Args: commandArgs(cobra.NoArgs), RunE: actionRun(authBrowserLogin)}
	login.Flags().Bool("no-browser", false, "print the approval link without opening a browser")
	login.Flags().Bool("reauth", false, "authenticate the current account again")
	login.Flags().Bool("change-account", false, "sign in with another account after browser approval")
	login.Flags().Bool("json", false, "print approval and sign-in states as JSON")
	login.Flags().Bool("fresh", false, "remove this computer's setup and machine record, then add it again")
	return login
}

// authCobraCommand declares `pb auth`: sign-in state and the active workspace.
func authCobraCommand() *cobra.Command {
	auth := &cobra.Command{Use: "auth", Short: "Manage Paperboat sign-in", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return command.Help()
		}
		return actionHomeAccount(command)
	}}
	status := &cobra.Command{Use: "status", Short: "Show the active Paperboat account", Args: commandArgs(cobra.NoArgs), RunE: actionRun(authStatus)}
	status.Flags().Bool("json", false, "")
	logout := &cobra.Command{Use: "logout", Short: "Revoke and remove the active client session", Args: commandArgs(cobra.NoArgs), RunE: actionRun(authLogout)}
	logout.Flags().Bool("json", false, "")
	auth.AddCommand(authLoginCommand(), status, logout, workspaceSwitchCommand())
	return auth
}
