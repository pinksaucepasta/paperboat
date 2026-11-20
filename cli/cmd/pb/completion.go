package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/spf13/cobra"
)

const shellCompletionDeadline = 200 * time.Millisecond

func shellCompletionContext(command *cobra.Command) context.Context {
	if command != nil && command.Context() != nil {
		return command.Context()
	}
	return context.Background()
}

func configureShellCompletion(root *cobra.Command) {
	if root == nil {
		return
	}
	machine := machineCompletion
	for _, path := range [][]string{{"connect"}, {"exec"}, {"ssh"}, {"ping"}, {"doctor"}, {"wait"}, {"machine", "revoke"}} {
		if command, _, err := root.Find(path); err == nil && command != nil {
			command.ValidArgsFunction = machine
		}
	}
	for _, path := range [][]string{{"session", "list"}} {
		if command, _, err := root.Find(path); err == nil && command != nil {
			command.ValidArgsFunction = sessionCompletion
		}
	}
	if command, _, err := root.Find([]string{"session", "attach"}); err == nil && command != nil {
		command.ValidArgsFunction = attachSessionCompletion
	}
	for _, parent := range []string{"session"} {
		for _, child := range []string{"rename", "close", "delete"} {
			if command, _, err := root.Find([]string{parent, child}); err == nil && command != nil {
				command.ValidArgsFunction = sessionCompletion
			}
		}
	}
	if command, _, err := root.Find([]string{"preview", "stop"}); err == nil && command != nil {
		command.ValidArgsFunction = previewStopCompletion
	}
	if command, _, err := root.Find([]string{"send"}); err == nil && command != nil {
		_ = command.RegisterFlagCompletionFunc("to", transferTargetCompletion)
		_ = command.RegisterFlagCompletionFunc("session", allSessionCompletion)
	}
	if command, _, err := root.Find([]string{"send", "destination", "set"}); err == nil && command != nil {
		command.ValidArgsFunction = transferTargetCompletion
	}
	root.ValidArgsFunction = personalizedMachineCompletion
}

const bashCompletionPrerequisite = "Bash completion requires Bash 4.2+ and bash-completion v2. On macOS: brew install bash bash-completion@2. Start the installed Bash and load bash_completion.sh before sourcing the pb completion script."

func configureBashCompletion(root *cobra.Command) {
	bash, _, err := root.Find([]string{"completion", "bash"})
	if err != nil || bash == nil {
		return
	}
	bash.Long += "\n\n" + bashCompletionPrerequisite + `

On macOS, after installing:
  exec "$(brew --prefix)/bin/bash"
  source "$(brew --prefix)/etc/profile.d/bash_completion.sh"
  source <(pb completion bash)
`
	bash.Run = nil
	bash.RunE = func(command *cobra.Command, _ []string) error {
		_, err := fmt.Fprintf(command.OutOrStdout(), `if (( BASH_VERSINFO[0] < 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] < 2) )) || { ! declare -F _init_completion >/dev/null 2>&1 && ! declare -F _get_comp_words_by_ref >/dev/null 2>&1; }; then
  printf '%%s\n' '%s' >&2
  return 1 2>/dev/null || exit 1
fi
`, bashCompletionPrerequisite)
		if err != nil {
			return err
		}
		noDescriptions, _ := command.Flags().GetBool("no-descriptions")
		return root.GenBashCompletionV2(command.OutOrStdout(), !noDescriptions)
	}
}

func machineCompletion(command *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return localCompletion(command, "machine", "", toComplete)
}

func transferTargetCompletion(command *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return localCompletion(command, "transfer_target", "", toComplete)
}

func sessionCompletion(command *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		return machineCompletion(command, nil, toComplete)
	}
	if len(args) > 1 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return sessionsForEnvironmentCompletion(command, args[0], toComplete)
}

func allSessionCompletion(command *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return localCompletion(command, "session", "", toComplete)
}

func attachSessionCompletion(command *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 1 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	if len(args) == 1 {
		return sessionsForEnvironmentCompletion(command, args[0], toComplete)
	}
	return localCompletionKinds(command, map[string]bool{"machine": true, "session": true}, "", toComplete)
}

func sessionsForEnvironmentCompletion(command *cobra.Command, environment, toComplete string) ([]string, cobra.ShellCompDirective) {
	return localCompletion(command, "session", environment, toComplete)
}

func localCompletion(command *cobra.Command, kind, environment, prefix string) ([]string, cobra.ShellCompDirective) {
	return localCompletionKinds(command, map[string]bool{kind: true}, environment, prefix)
}

func localCompletionKinds(command *cobra.Command, kinds map[string]bool, environment, prefix string) ([]string, cobra.ShellCompDirective) {
	ctx, cancel := context.WithTimeout(shellCompletionContext(command), shellCompletionDeadline)
	defer cancel()
	paths, err := hostDependenciesOf(ctx).daemonPaths()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	client, err := localapi.NewClient(paths.SocketPath, shellCompletionDeadline)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	snapshot, err := client.Completions(ctx)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	environmentID := ""
	if environment != "" {
		for _, item := range snapshot.Items {
			if item.Kind == "machine" && strings.EqualFold(item.Value, environment) {
				environmentID = item.EnvironmentID
				break
			}
		}
		if environmentID == "" {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
	}
	values := make([]string, 0)
	seen := make(map[string]bool)
	for _, item := range snapshot.Items {
		if !kinds[item.Kind] || environmentID != "" && item.EnvironmentID != environmentID || seen[item.Value] || !strings.HasPrefix(strings.ToLower(item.Value), strings.ToLower(prefix)) {
			continue
		}
		seen[item.Value] = true
		values = append(values, item.Value+"\t"+item.Description)
	}
	sort.Strings(values)
	return values, cobra.ShellCompDirectiveNoFileComp
}

func sessionCompletionValues(items []api.TerminalSession, prefix string) []string {
	values := make([]string, 0, len(items)*2)
	seen := make(map[string]bool)
	for _, item := range items {
		for _, value := range []string{item.Name, item.ID} {
			if value == "" || seen[value] || !strings.HasPrefix(strings.ToLower(value), strings.ToLower(prefix)) {
				continue
			}
			seen[value] = true
			values = append(values, value+"\t"+item.State)
		}
	}
	sort.Strings(values)
	return values
}
