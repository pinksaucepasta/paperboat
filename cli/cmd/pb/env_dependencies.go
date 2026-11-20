package main

import (
	"context"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/environmentmanager"
	"github.com/spf13/cobra"
)

// envDependencies are the edges the ENV, vault and recording commands reach outside the process through: the
// authenticated backend, machine lookup, the unlocked vault, the hidden password prompt and the hook's terminal.
type envDependencies struct {
	backendForCommand func(*cobra.Command) (*api.Client, error)
	resolveMachine    func(context.Context, *api.Client, string) (api.UserMachine, error)
	passwordVault     func(*cobra.Command) (environmentmanager.PasswordVault, error)
	passwordPrompt    func(command *cobra.Command, title string) ([]byte, error)
	namedVaultSession func(*cobra.Command) (namedVaultSession, error)
	// hookTerminal is where the hook's one-line notice goes. The hook line discards pb's stderr so an old or
	// broken pb never adds noise to shell startup; the notice goes to the controlling terminal, which
	// non-interactive ssh and VS Code's environment probe do not have.
	hookTerminal string
}

func defaultEnvDependencies() envDependencies {
	return envDependencies{
		backendForCommand: backendForCommand,
		resolveMachine:    resolveUserMachine,
		passwordVault:     defaultPasswordVault,
		passwordPrompt:    defaultPasswordPrompt,
		namedVaultSession: defaultNamedVaultSession,
		hookTerminal:      envHookConsole,
	}
}

func envDependenciesOf(ctx context.Context) envDependencies {
	return dependenciesIn(ctx, defaultEnvDependencies)
}
