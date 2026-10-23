package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

// accountCobraCommand groups account commands. Deleting an account is console-only (user decision
// 2026-10-23): pb only shows where to do it.
func accountCobraCommand() *cobra.Command {
	account := &cobra.Command{Use: "account", Short: "Manage your Paperboat account", Long: "Commands for your Paperboat account itself, as opposed to its machines, teams or sessions. Deleting the account removes everything it owns and is done only in the Paperboat console, where you confirm it with your email; `pb account delete` prints the link.", Args: commandArgs(cobra.NoArgs)}
	account.AddCommand(accountDeleteCommand())
	return account
}

func accountDeleteCommand() *cobra.Command {
	c := &cobra.Command{Use: "delete", Short: "Show where to delete your Paperboat account", Long: "Deleting your Paperboat account removes your machines, sessions and data and cannot be undone. It is done in the Paperboat console, where you confirm it with your email; pb prints the link.", Args: commandArgs(cobra.NoArgs), RunE: func(c *cobra.Command, _ []string) error {
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		configuration, err := client.ClientConfiguration(c.Context())
		if err != nil {
			return err
		}
		link, err := accountDeletionLink(configuration.ConsoleURL)
		if err != nil {
			return err
		}
		if asJSON, _ := c.Flags().GetBool("json"); asJSON {
			return writeCLIJSON(c.OutOrStdout(), map[string]any{"console_url": link})
		}
		_, err = fmt.Fprintf(c.OutOrStdout(), "Delete your Paperboat account in the console (you confirm it there with your email):\n%s\n", link)
		return err
	}}
	c.Flags().Bool("json", false, "print JSON")
	return c
}

// accountDeletionLink is the console's account settings page.
func accountDeletionLink(consoleURL string) (string, error) {
	console, err := url.Parse(strings.TrimRight(consoleURL, "/"))
	if err != nil || console.Host == "" || console.Scheme != "https" && console.Scheme != "http" || console.User != nil {
		return "", errors.New("the Paperboat server did not publish its console address; update pb or contact the server operator")
	}
	return console.String() + "/settings/account", nil
}
