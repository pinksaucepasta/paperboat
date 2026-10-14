package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"

	documentation "github.com/pinksaucepasta/paperboat/docs"
	"github.com/spf13/cobra"
)

// removeInstalledManuals removes the Linux pages that install wrote to
// Dir(~/.local/bin)/share/man. The running executable is not used: it resolves
// through the ~/.local/bin/pb symlink to the system copy, which has the wrong
// share directory.
func removeInstalledManuals(home string) error {
	// macOS system manuals are removed by privileged purge after the last
	// account CLI/runtime goes away; this user-side step owns only Linux pages.
	if runtime.GOOS != "linux" {
		return nil
	}
	return documentation.Remove(filepath.Join(home, ".local", "share", "man"))
}

// This installer helper extracts only bundled data. It never downloads manuals
// or evaluates user preferences; installers pass --no-customization explicitly.
func manualInstallCommand() *cobra.Command {
	command := &cobra.Command{Use: "__man-pages", Hidden: true, Args: cobra.NoArgs}
	command.Flags().String("directory", "", "manual root containing man1")
	command.Flags().Bool("remove", false, "remove owned manual pages")
	command.RunE = func(c *cobra.Command, _ []string) error {
		directory, _ := c.Flags().GetString("directory")
		if directory == "" {
			return invocationError(errors.New("--directory is required"))
		}
		remove, _ := c.Flags().GetBool("remove")
		var err error
		if remove {
			err = documentation.Remove(directory)
		} else {
			err = documentation.Install(directory)
		}
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(c.OutOrStdout(), directory)
		return err
	}
	return command
}
