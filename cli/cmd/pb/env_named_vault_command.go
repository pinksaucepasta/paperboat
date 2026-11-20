package main

import (
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	env "github.com/pinksaucepasta/paperboat/cli/internal/environmente2ee"
	"github.com/pinksaucepasta/paperboat/cli/internal/environmentmanager"
	"github.com/pinksaucepasta/paperboat/shared/recipe"
	"github.com/spf13/cobra"
)

// Named ENV vaults (PLAN F26): a vault is a named set of values with its own key, readable by the
// workspace's members and by the machines of the recipes that list it. Keys are made, wrapped and
// used only on members' computers and recipe machines; Paperboat keeps ciphertext.

// namedVaultSession is what every vault command needs: the API client, the workspace ("personal" or
// a team id) and the unlocked ENV keys.
type namedVaultSession struct {
	client    *api.Client
	workspace string
	team      string
	account   string
	vault     environmentmanager.PasswordVault
}

func defaultNamedVaultSession(command *cobra.Command) (namedVaultSession, error) {
	client, workspace, err := policyClient(command, nil)
	if err != nil {
		return namedVaultSession{}, err
	}
	vault, err := envDependenciesOf(command.Context()).passwordVault(command)
	if err != nil {
		return namedVaultSession{}, err
	}
	team := ""
	if workspace != "personal" {
		team = workspace
	}
	return namedVaultSession{client: client, workspace: workspace, team: team, account: vault.AccountID, vault: vault}, nil
}

// withKeys runs fn with the unlocked ENV keys, naming the unlock command when they are locked.
func (s namedVaultSession) withKeys(command *cobra.Command, why string, fn func(*env.VaultKeys) error) error {
	err := s.vault.WithUnlockedKeys(command.Context(), fn)
	if errors.Is(err, environmentmanager.ErrVaultLocked) {
		return localArgumentError(why + ", and your ENV keys are locked on this computer. Unlock them with pb env unlock, then run the command again. The vault key is wrapped here, on your computer; Paperboat never sees it.")
	}
	return err
}

func toNamedWraps(wraps []api.EnvVaultWrap) []env.NamedVaultWrap {
	out := make([]env.NamedVaultWrap, len(wraps))
	for i, w := range wraps {
		out[i] = env.NamedVaultWrap{Kind: w.Kind, ID: w.ID, Wrapped: w.Wrapped}
	}
	return out
}

func addNamedVaultCommands(root *cobra.Command) {
	vault := &cobra.Command{Use: "vault", Short: "Create, list and delete named ENV vaults that recipes read", Long: `A named vault is a set of ENV values with its own key, for example canary-env. Members
of the workspace read it with their ENV keys, and machines paired from a recipe whose
[env] vaults lists it read it with the recipe's token, with nobody online. Set values
with pb env set <name> --vault <vault>.`, Args: commandArgs(cobra.NoArgs)}

	create := &cobra.Command{Use: "create <vault>", Short: "Create a named ENV vault", Long: `Create a named vault in the active workspace. Its key is made on this computer and
wrapped with your ENV keys (and the team's, in a team workspace), so unlock them first
with pb env unlock. Names use lowercase letters, digits and -, up to 63 characters.`, Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		name := args[0]
		if !recipe.ValidName(name) {
			return localArgumentError("a vault name uses lowercase letters, digits and -, up to 63 characters, like canary-env")
		}
		s, err := envDependenciesOf(command.Context()).namedVaultSession(command)
		if err != nil {
			return err
		}
		key, err := recipe.NewVaultKey()
		if err != nil {
			return err
		}
		defer clear(key)
		var wraps []env.NamedVaultWrap
		if err := s.withKeys(command, "creating vault "+name+" wraps its key", func(keys *env.VaultKeys) error {
			wraps, err = env.NamedVaultWraps(keys, s.account, s.team, name, key)
			return err
		}); err != nil {
			return err
		}
		apiWraps := make([]api.EnvVaultWrap, len(wraps))
		for i, w := range wraps {
			apiWraps[i] = api.EnvVaultWrap{Kind: w.Kind, ID: w.ID, Wrapped: w.Wrapped}
		}
		created, err := s.client.CreateEnvVault(command.Context(), s.workspace, name, apiWraps)
		if err != nil {
			return friendlyCommandError(err)
		}
		if wantsJSON(command) {
			return writeCLIJSON(command.OutOrStdout(), map[string]any{"vault": created})
		}
		_, err = fmt.Fprintf(command.OutOrStdout(), "Vault %s created. Add values with pb env set <name> --vault %s.\n", name, name)
		return err
	}}
	create.Flags().Bool("json", false, "print JSON")

	list := &cobra.Command{Use: "list", Short: "List the workspace's named ENV vaults", Long: `List the named vaults of the active workspace with their key epoch, value revision and
the recipes that read them. Values are never printed; pb env run shows them to a
command on a machine that may read them.`, Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
		client, workspace, err := policyClient(command, args)
		if err != nil {
			return err
		}
		vaults, err := client.EnvVaults(command.Context(), workspace)
		if err != nil {
			return friendlyCommandError(err)
		}
		if wantsJSON(command) {
			return writeCLIJSON(command.OutOrStdout(), map[string]any{"vaults": vaults})
		}
		if len(vaults) == 0 {
			_, err = fmt.Fprintln(command.OutOrStdout(), "No vaults. Create one with pb env vault create <vault>.")
			return err
		}
		writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 3, ' ', 0)
		fmt.Fprintln(writer, "VAULT\tKEY\tREVISION\tRECIPES")
		for _, v := range vaults {
			fmt.Fprintf(writer, "%s\t%d\t%d\t%s\n", v.Name, v.KeyEpoch, v.Revision, firstNonEmpty(strings.Join(v.Recipes, ","), "-"))
		}
		return writer.Flush()
	}}
	list.Flags().Bool("json", false, "print JSON")

	remove := &cobra.Command{Use: "delete <vault>", Short: "Delete a named ENV vault and its values", Long: `Delete a named vault and its values. A vault a recipe still reads cannot be deleted:
remove it from the recipe's [env] vaults and run pb recipe update first.`, Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		client, workspace, err := policyClient(command, args)
		if err != nil {
			return err
		}
		if err := client.DeleteEnvVault(command.Context(), workspace, args[0]); err != nil {
			return friendlyCommandError(err)
		}
		if wantsJSON(command) {
			return writeCLIJSON(command.OutOrStdout(), map[string]any{"vault": args[0], "deleted": true})
		}
		_, err = fmt.Fprintf(command.OutOrStdout(), "Vault %s deleted.\n", args[0])
		return err
	}}
	remove.Flags().Bool("json", false, "print JSON")

	vault.AddCommand(create, list, remove)
	root.AddCommand(vault)
}

// changeNamedVaultValue sets (value != nil) or removes one name in a named vault: the vault key is
// opened with this account's keys, the values are decrypted, changed and sealed again here.
func changeNamedVaultValue(command *cobra.Command, vaultName, name string, value []byte) error {
	// A name already in the vault can always be removed, as in the browser; only a name being set is checked.
	if value != nil {
		if err := environmentVariableNameError(name); err != nil {
			return err
		}
	}
	s, err := envDependenciesOf(command.Context()).namedVaultSession(command)
	if err != nil {
		return err
	}
	v, err := s.client.GetEnvVault(command.Context(), s.workspace, vaultName)
	if err != nil {
		return friendlyCommandError(err)
	}
	var envelope []byte
	if err := s.withKeys(command, "vault "+vaultName+" is encrypted", func(keys *env.VaultKeys) error {
		key, err := env.OpenNamedVaultKey(keys, s.account, vaultName, toNamedWraps(v.Wraps))
		if err != nil {
			return err
		}
		defer clear(key)
		values, err := recipe.OpenValues(key, vaultName, v.KeyEpoch, v.Envelope)
		if err != nil {
			return err
		}
		if value == nil {
			delete(values, name)
		} else {
			values[name] = string(value)
		}
		envelope, err = recipe.SealValues(key, vaultName, v.KeyEpoch, values)
		return err
	}); err != nil {
		return err
	}
	if _, err := s.client.PutEnvVaultValues(command.Context(), s.workspace, vaultName, v.Revision, v.KeyEpoch, envelope); err != nil {
		return friendlyCommandError(err)
	}
	return nil
}

// recipeVaultWraps wraps the key of each vault a recipe reads to the recipe's public key, on this
// computer, for pb recipe create and update.
func recipeVaultWraps(command *cobra.Command, recipeName string, vaults []string, public []byte, tokenID string) ([]api.RecipeVaultWrap, error) {
	if len(vaults) == 0 {
		return nil, nil
	}
	s, err := envDependenciesOf(command.Context()).namedVaultSession(command)
	if err != nil {
		return nil, err
	}
	stored := make([]api.EnvVault, 0, len(vaults))
	for _, name := range vaults {
		v, err := s.client.GetEnvVault(command.Context(), s.workspace, name)
		if err != nil {
			// An unknown vault is refused by the server with the workspace's vault names.
			var apiErr *api.APIError
			if errors.As(err, &apiErr) && apiErr.Status == 404 {
				continue
			}
			return nil, friendlyCommandError(err)
		}
		stored = append(stored, v)
	}
	if len(stored) == 0 {
		return nil, nil
	}
	out := make([]api.RecipeVaultWrap, 0, len(stored))
	err = s.withKeys(command, fmt.Sprintf("recipe %s reads vault %s", recipeName, strings.Join(vaults, ", ")), func(keys *env.VaultKeys) error {
		for _, v := range stored {
			wrapped, err := env.WrapNamedVaultForRecipe(keys, s.account, v.Name, toNamedWraps(v.Wraps), public, tokenID)
			if err != nil {
				return fmt.Errorf("wrap vault %s for recipe %s: %w", v.Name, recipeName, err)
			}
			out = append(out, api.RecipeVaultWrap{Vault: v.Name, Wrapped: wrapped})
		}
		return nil
	})
	return out, err
}
