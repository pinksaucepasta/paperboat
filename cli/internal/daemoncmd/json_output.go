package daemoncmd

import (
	"encoding/json"
	"fmt"
	"github.com/spf13/cobra"
)

func writeDaemonCommandResult(command *cobra.Command, data any, message string) error {
	jsonOutput, _ := command.Flags().GetBool("json")
	if jsonOutput {
		err := json.NewEncoder(command.OutOrStdout()).Encode(struct {
			SchemaVersion string `json:"schema_version"`
			OK            bool   `json:"ok"`
			Data          any    `json:"data"`
		}{"1.0", true, data})
		return coordinatorFailure("Could not write the daemon command result; retry with writable output.", err)
	}
	_, err := fmt.Fprintln(command.OutOrStdout(), message)
	return coordinatorFailure("Could not write the daemon command result; retry with writable output.", err)
}
