package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/spf13/cobra"
)

// machineTagCommand edits one machine's tags in the workspace policy file (PLAN F18): each change
// is a new policy version, reviewed like pb policy set when it changes anyone's access.
func machineTagCommand() *cobra.Command {
	command := &cobra.Command{Use: "tag <machine> [add|remove|set] [tag...]", Short: "Show or change a machine's tags", Long: `Show a machine's tags, or change them in the active workspace's policy: add
or remove tags, or set the full list. Tags are tag:<name> (the tag: prefix may be
left out). A tag that does not exist yet is created; a tag left with no machines
is removed unless a rule uses it. A change that gives or takes anyone's access
(rules on the tag) is printed and must be confirmed; --yes confirms it. Members
may tag only machines they shared.`, Example: "  pb machine tag db\n  pb machine tag db add prod ci\n  pb machine tag db remove ci --yes\n  pb machine tag db set prod",
		Args: commandArgs(cobra.MinimumNArgs(1)), RunE: func(command *cobra.Command, args []string) error {
			client, workspace, err := policyClient(command, args)
			if err != nil {
				return err
			}
			machine, err := resolveUserMachine(command.Context(), client, args[0])
			if err != nil {
				return friendlyCommandError(err)
			}
			if len(args) == 1 {
				return printMachineTags(command, machine.Alias, machine.Tags, 0)
			}
			next, err := nextMachineTags(machine.Tags, args[1], args[2:])
			if err != nil {
				return err
			}
			current, err := client.GetPolicy(command.Context(), workspace, 0)
			if err != nil {
				return friendlyCommandError(err)
			}
			request := api.MachineTagsRequest{Tags: next, ExpectedVersion: current.Version}
			out, err := client.SetMachineTags(command.Context(), workspace, machine.ID, request)
			if details, ok := api.PolicyDetails(err, "policy_changes_unconfirmed"); ok && details.Confirm != "" {
				printPolicyChanges(command.OutOrStdout(), details.AccessChanges)
				if err := confirmAccessChanges(command); err != nil {
					return err
				}
				request.Confirm = details.Confirm
				out, err = client.SetMachineTags(command.Context(), workspace, machine.ID, request)
			}
			if details, ok := api.PolicyDetails(err, "invalid_policy"); ok {
				printPolicyErrors(command.ErrOrStderr(), details.Errors)
			}
			if err != nil {
				return friendlyCommandError(err)
			}
			return printMachineTags(command, machine.Alias, out.Tags, out.PolicyVersion)
		}}
	command.Flags().Bool("yes", false, "confirm the printed access changes without asking")
	command.Flags().Bool("json", false, "print JSON")
	return command
}

// nextMachineTags applies add, remove or set to current; names get the tag: prefix and are checked.
func nextMachineTags(current []string, verb string, names []string) ([]string, error) {
	var given []string
	for _, n := range names {
		tag := "tag:" + strings.TrimPrefix(strings.TrimSpace(n), "tag:")
		if !validTagName(strings.TrimPrefix(tag, "tag:")) {
			return nil, localArgumentError(fmt.Sprintf("invalid tag %q: a tag is tag:<name> with lowercase letters, digits, '.', '_' or '-'", n))
		}
		given = append(given, tag)
	}
	var next []string
	switch verb {
	case "add":
		next = append(slices.Clone(current), given...)
	case "remove":
		next = slices.DeleteFunc(slices.Clone(current), func(t string) bool { return slices.Contains(given, t) })
	case "set":
		next = given
	default:
		return nil, localArgumentError("say add, remove or set after the machine, then the tags")
	}
	if verb != "set" && len(given) == 0 {
		return nil, localArgumentError("name at least one tag to " + verb)
	}
	slices.Sort(next)
	return slices.Compact(next), nil
}

// validTagName is shared/policy's tag name rule (the server checks it again).
func validTagName(name string) bool {
	if name == "" || len(name) > 63 || name[0] == '.' || name[0] == '_' || name[0] == '-' {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func printMachineTags(command *cobra.Command, alias string, tags []string, version int64) error {
	if wantsJSON(command) {
		out := map[string]any{"machine": alias, "tags": nonNilTags(tags)}
		if version > 0 {
			out["policy_version"] = version
		}
		return writeJSON(command.OutOrStdout(), out)
	}
	list := "no tags"
	if len(tags) > 0 {
		list = strings.Join(tags, ", ")
	}
	if version > 0 {
		_, err := fmt.Fprintf(command.OutOrStdout(), "Tags of %s: %s (policy version %d).\n", alias, list, version)
		return err
	}
	_, err := fmt.Fprintf(command.OutOrStdout(), "Tags of %s: %s.\n", alias, list)
	return err
}

func nonNilTags(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return tags
}

// machineTagFilter keeps machines carrying every --tag (prefix optional).
func machineTagFilter(command *cobra.Command, machines []api.UserMachine) ([]api.UserMachine, error) {
	wanted, _ := command.Flags().GetStringArray("tag")
	if len(wanted) == 0 {
		return machines, nil
	}
	for i, w := range wanted {
		wanted[i] = "tag:" + strings.TrimPrefix(w, "tag:")
		if !validTagName(strings.TrimPrefix(wanted[i], "tag:")) {
			return nil, localArgumentError(fmt.Sprintf("invalid --tag %q", w))
		}
	}
	out := machines[:0:0]
	for _, m := range machines {
		if !slices.ContainsFunc(wanted, func(t string) bool { return !slices.Contains(m.Tags, t) }) {
			out = append(out, m)
		}
	}
	return out, nil
}
