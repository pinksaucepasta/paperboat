package main

import (
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/spf13/cobra"
)

// versionCommand makes `pb version` print the version like `pb --version`; without it the word was
// taken as a machine name ("No machine has that name or ID").
func versionCommand() *cobra.Command {
	c := &cobra.Command{Use: "version", Short: "Print the pb version", Long: "Print this pb's version, the same as `pb --version`. With --json it prints the version and the source commit it was built from, which is what support and bug reports ask for.", Args: commandArgs(cobra.NoArgs), RunE: func(c *cobra.Command, _ []string) error {
		if asJSON, _ := c.Flags().GetBool("json"); asJSON {
			return writeCLIJSON(c.OutOrStdout(), map[string]string{"version": buildinfo.Version, "commit": buildinfo.Commit})
		}
		_, err := c.OutOrStdout().Write([]byte(versionDisplay(buildinfo.Version)))
		return err
	}}
	c.Flags().Bool("json", false, "print JSON")
	return c
}
