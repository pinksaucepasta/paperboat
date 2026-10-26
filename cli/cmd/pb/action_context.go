package main

import (
	"flag"

	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Copy the parsed command's flags, rather than maintaining a second flag list
// which can silently drop valid options before their owning action sees them.
func actionContext(cobraCommand *cobra.Command, args []string) *command.Context {
	set := flag.NewFlagSet("pb", flag.ContinueOnError)
	cobraCommand.Flags().VisitAll(func(source *pflag.Flag) {
		switch source.Value.Type() {
		case "bool":
			value, _ := cobraCommand.Flags().GetBool(source.Name)
			set.Bool(source.Name, value, "")
		case "uint":
			value, _ := cobraCommand.Flags().GetUint(source.Name)
			set.Uint(source.Name, value, "")
		default:
			set.String(source.Name, source.Value.String(), "")
		}
	})
	set.Bool("duration-set", cobraCommand.Flags().Changed("duration"), "")
	_ = set.Parse(args)
	result := command.NewContext(set, cobraCommand.InOrStdin())
	result.Context = cobraCommand.Context()
	result.Writer = cobraCommand.OutOrStdout()
	result.ErrWriter = cobraCommand.ErrOrStderr()
	return result
}
