package main

import (
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/spf13/cobra"
	"net/url"
	"strconv"
)

func inventoryFilterFlags(command *cobra.Command) {
	command.Flags().String("q", "", "filter by resource name or ID")
	command.Flags().String("state", "", "filter by resource state")
	command.Flags().String("owner", "", "filter owner: mine, shared, or an authorized account ID")
}
func inventoryFilters(command *cobra.Command) url.Values {
	values := url.Values{}
	for _, key := range []string{"q", "state", "owner"} {
		value, _ := command.Flags().GetString(key)
		if value != "" {
			values.Set(key, value)
		}
	}
	if flag := command.Flags().Lookup("closed"); flag != nil && flag.Changed {
		values.Set("closed", flag.Value.String())
	}
	return values
}
func writeInventoryContinuation(command *cobra.Command, operands []string, cursor string, limit int) error {
	if cursor == "" {
		return nil
	}
	args := append([]string{}, operands...)
	args = append(args, "--cursor", cursor, "--limit", strconv.Itoa(limit))
	for _, key := range []string{"q", "state", "owner"} {
		if value := inventoryFilters(command).Get(key); value != "" {
			args = append(args, "--"+key, value)
		}
	}
	_, err := fmt.Fprintf(command.OutOrStdout(), "Next page: %s %s\n", command.CommandPath(), formatPreferenceArgs(args))
	return err
}

func commandInventoryFilters(ctx *command.Context) url.Values {
	if selected, ok := ctx.Context.Value(confirmationCommandKey{}).(*cobra.Command); ok {
		return inventoryFilters(selected)
	}
	return nil
}

func homeDoctorDetail(choice selector.Item) string {
	recovery := "Run pb doctor --json for the full diagnostic report."
	switch choice.ID {
	case "setup", "identity", "credential":
		recovery += " If this machine is not enrolled or its identity is unavailable, run pb setup and complete dashboard enrollment."
	case "inbox":
		recovery += " Check that the displayed inbox directory exists and this account can access it."
	case "runtime", "workloads":
		recovery += " Inspect the reported worker state and use the machine setup/repair flow when a required service is stopped."
	case "auth":
		recovery += " Run pb auth status; use Account → Sign in if your session has expired."
	case "backend":
		recovery += " Check Configuration → Paperboat server and network access, then retry Diagnostics."
	}
	return choice.Description + "\n\n" + recovery
}
