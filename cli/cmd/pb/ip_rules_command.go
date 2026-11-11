package main

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/spf13/cobra"
)

// ipRulesCommand shows or changes a tunnel's or preview's owner IP lists (edge abuse plan 3.5). A
// list given by flags replaces that list; the other is kept. --clear removes both.
func ipRulesCommand(kind string) *cobra.Command {
	command := &cobra.Command{Use: "ip-rules <" + kind + ">", Short: "Show or set which client addresses may open this " + kind,
		Long: "Without flags, show the " + kind + "'s IP allow and deny lists. --allow and --deny take an address or CIDR and repeat; each replaces that whole list. " +
			"An address a deny entry matches is refused, and when the allow list is not empty only its addresses get in. Refused visitors see 403 \"Access to this link is restricted\".",
		Args: commandArgs(cobra.ExactArgs(1)), SilenceUsage: true, SilenceErrors: true,
		RunE: func(command *cobra.Command, args []string) error {
			allow, _ := command.Flags().GetStringArray("allow")
			deny, _ := command.Flags().GetStringArray("deny")
			clear, _ := command.Flags().GetBool("clear")
			setAllow, setDeny := command.Flags().Changed("allow"), command.Flags().Changed("deny")
			if clear && (setAllow || setDeny) {
				return invocationError(errors.New("--clear cannot be combined with --allow or --deny"))
			}
			for _, entry := range append(append([]string{}, allow...), deny...) {
				if !validIPRuleEntry(entry) {
					return invocationError(fmt.Errorf("%q is not an IP address or CIDR", entry))
				}
			}
			client, err := tunnelClientForCommand(command)
			if err != nil {
				return err
			}
			id, resource := args[0], kind+"s"
			if kind == "tunnel" {
				if id, err = resolveTunnelSelectorForCommand(command.Context(), client, args[0]); err != nil {
					return err
				}
			} else {
				lease, err := client.GetPreviewLease(command.Context(), args[0])
				if err != nil {
					return err
				}
				id = lease.ID
			}
			rules, err := client.IPRules(command.Context(), resource, id)
			if err != nil {
				return err
			}
			if clear || setAllow || setDeny {
				if clear {
					rules = api.IPRules{}
				}
				if setAllow {
					rules.Allow = allow
				}
				if setDeny {
					rules.Deny = deny
				}
				if rules, err = client.PutIPRules(command.Context(), resource, id, rules); err != nil {
					return err
				}
			}
			return tunnelOutput(command, rules, ipRulesText(rules))
		}}
	command.Flags().StringArray("allow", nil, "address or CIDR allowed in (repeatable; replaces the allow list)")
	command.Flags().StringArray("deny", nil, "address or CIDR refused (repeatable; replaces the deny list)")
	command.Flags().Bool("clear", false, "remove both lists")
	command.Flags().Bool("json", false, "print the lists as JSON")
	return command
}

func validIPRuleEntry(entry string) bool {
	if _, err := netip.ParsePrefix(entry); err == nil {
		return true
	}
	_, err := netip.ParseAddr(entry)
	return err == nil
}

func ipRulesText(rules api.IPRules) string {
	if len(rules.Allow) == 0 && len(rules.Deny) == 0 {
		return "No IP rules: every address may open it."
	}
	allow := "every address not denied"
	if len(rules.Allow) > 0 {
		allow = strings.Join(rules.Allow, ", ")
	}
	deny := "none"
	if len(rules.Deny) > 0 {
		deny = strings.Join(rules.Deny, ", ")
	}
	return "Allow: " + allow + "\nDeny:  " + deny
}
