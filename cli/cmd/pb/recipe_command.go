package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/shared/recipe"
	"github.com/spf13/cobra"
)

// pb recipe (PLAN F26): a recipe is a small file that says what a machine becomes when it pairs.
func recipeCobraCommand() *cobra.Command {
	root := &cobra.Command{Use: "recipe", Short: "Create and manage recipes that pair and set up machines", Long: `A recipe is a small TOML or JSON file that says what happens on a machine when it
joins: its tags, what it may reach, which ENV vaults it reads, which repository it
pulls config from, which preview or tunnel it opens, and when it is removed. Create
one with pb recipe create -f ci.toml; the printed token, set as PB_RECIPE, pairs and
sets up a machine with one command and no prompt.`, Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error { return command.Help() }}
	root.AddCommand(recipeCreateCommand(), recipeListCommand(), recipeShowCommand(), recipeUpdateCommand(), recipeRotateCommand(), recipeRevokeCommand())
	return root
}

// recipeFileRefusal is a problem in the recipe file: its own message (file:line:col), without the
// command help, which says nothing about the file.
func recipeFileRefusal(message string) error {
	return usageError{err: errors.New(message), publicMessage: message, refusal: true}
}

// readRecipeFile reads and validates a recipe file on this computer, with file:line:col errors.
func readRecipeFile(path string) (name, format, source string, parsed recipe.Recipe, err error) {
	if name, err = recipe.NameFromFile(path); err != nil {
		return "", "", "", parsed, recipeFileRefusal(err.Error())
	}
	file, err := os.Open(path)
	if err != nil {
		return "", "", "", parsed, recipeFileRefusal(fmt.Sprintf("read the recipe file: %v", err))
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, recipe.MaxSourceBytes+1))
	if err != nil {
		return "", "", "", parsed, recipeFileRefusal(fmt.Sprintf("read the recipe file: %v", err))
	}
	if parsed, err = recipe.Parse(path, data); err != nil {
		return "", "", "", parsed, recipeFileRefusal(err.Error())
	}
	return name, strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), "."), string(data), parsed, nil
}

func recipeFileFlag(command *cobra.Command) (string, error) {
	path, _ := command.Flags().GetString("file")
	if strings.TrimSpace(path) == "" {
		return "", localArgumentError("give the recipe file with -f, like pb recipe " + command.Name() + " -f ci.toml")
	}
	return path, nil
}

func recipeCreateCommand() *cobra.Command {
	command := &cobra.Command{Use: "create -f <file>", Short: "Create a recipe from a file and print its token once", Long: `Create a recipe from a TOML or JSON file; its name is the file's base name (ci.toml
makes ci). The file is checked on this computer first, with file:line:col errors.
The token is made here and printed once: save it as the secret PB_RECIPE. Paperboat
stores only a hash of it and can never read the ENV vaults the recipe reads.`, Example: "  pb recipe create -f ci.toml\n  pb recipe create -f ci.toml --workspace acme", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
		path, err := recipeFileFlag(command)
		if err != nil {
			return err
		}
		name, format, source, parsed, err := readRecipeFile(path)
		if err != nil {
			return err
		}
		client, workspace, err := policyClient(command, args)
		if err != nil {
			return err
		}
		token, err := recipe.NewToken("")
		if err != nil {
			return err
		}
		wraps, err := recipeVaultWraps(command, name, parsed.Vaults(), token.PublicKey(), token.ID)
		if err != nil {
			return err
		}
		created, err := client.CreateRecipe(command.Context(), workspace, api.RecipeCreate{Name: name, Source: source, Format: format, TokenID: token.ID, Auth: token.Auth(), PublicKey: token.PublicKey(), VaultWraps: wraps})
		if err != nil {
			return friendlyCommandError(err)
		}
		install, err := recipeInstallLines(command, client)
		if err != nil {
			return err
		}
		if wantsJSON(command) {
			return writeCLIJSON(command.OutOrStdout(), map[string]any{"recipe": created, "token": token.String(), "install": install})
		}
		out := command.OutOrStdout()
		fmt.Fprintf(out, "Recipe %s created in workspace %s.\n", created.Name, created.Workspace)
		writeRecipeSummary(out, parsed, created)
		fmt.Fprintf(out, "\nToken (shown once; save it as the secret PB_RECIPE):\n  %s\n\nInstall, with PB_RECIPE set:\n  Linux, macOS  %s\n  Windows       %s\n  pb installed  pb pair\n", token, install["linux_macos"], install["windows"])
		return nil
	}}
	command.Flags().StringP("file", "f", "", "the recipe file (.toml or .json)")
	command.Flags().Bool("json", false, "print JSON (the token included, once)")
	return command
}

// recipeInstallLines are the installer one-liners from server discovery; the token never goes in a URL.
func recipeInstallLines(command *cobra.Command, client *api.Client) (map[string]string, error) {
	discovery, err := client.ClientConfiguration(command.Context())
	if err != nil {
		return nil, friendlyCommandError(err)
	}
	base, err := installerBase(discovery.InstallerBaseURL)
	if err != nil {
		return nil, err
	}
	// PB_RECIPE is read from the environment (a CI secret, say), so the lines carry no token.
	return map[string]string{"linux_macos": posixInstallCommand(base), "windows": windowsInstallCommand(base)}, nil
}

func writeRecipeSummary(out io.Writer, r recipe.Recipe, stored api.Recipe) {
	row := func(label, value string) { fmt.Fprintf(out, "  %-11s %s\n", label, value) }
	row("Tags", firstNonEmpty(strings.Join(r.Tags, ", "), "none"))
	if len(r.Reach) == 0 {
		row("Reach", "nothing (machines from this recipe cannot use other machines)")
	}
	for _, reach := range r.Reach {
		ports := ""
		for i, p := range reach.Ports {
			ports += map[bool]string{true: ":", false: ","}[i == 0] + p.String()
		}
		row("Reach", strings.Join(reach.Allow, ", ")+" on "+strings.Join(reach.Dst, ", ")+ports)
	}
	if vaults := r.Vaults(); len(vaults) > 0 {
		row("ENV", strings.Join(vaults, ", ")+" (key wrapped on this computer; Paperboat cannot read it)")
	}
	if r.Config != nil {
		row("Config", "pulled from repository "+r.Config.Repository)
	}
	for _, p := range r.Preview {
		row("Preview", fmt.Sprintf("port %d for %s", p.Port, p.TTL))
	}
	for _, t := range r.Tunnel {
		target := t.From
		if t.Port != 0 {
			target = fmt.Sprintf("port %d", t.Port)
		}
		row("Tunnel", firstNonEmpty(t.Name, "named after the machine")+" -> "+target)
	}
	machines := "no limit"
	if r.MaxMachines > 0 {
		machines = fmt.Sprintf("up to %d", r.MaxMachines)
	}
	if r.RemoveAfterOffline != "" {
		machines += "; removed " + r.RemoveAfterOffline + " after going offline"
	}
	if r.MachineLifetime != "" {
		machines += "; each removed " + r.MachineLifetime + " after pairing"
	}
	row("Machines", machines)
	row("Expires", fmt.Sprintf("%s (%s)", stored.ExpiresAt.Local().Format("2006-01-02"), r.Expires))
}

func recipeListCommand() *cobra.Command {
	command := &cobra.Command{Use: "list", Short: "List the workspace's recipes", Long: `List the recipes of the active workspace (or --workspace) with their tags, how many
machines each has paired (of its max_machines), when its token expires and when it
was last used. Tokens are never shown again after create and rotate; the list prints
only their public id.`, Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
		client, workspace, err := policyClient(command, args)
		if err != nil {
			return err
		}
		recipes, err := client.Recipes(command.Context(), workspace)
		if err != nil {
			return friendlyCommandError(err)
		}
		if wantsJSON(command) {
			return writeCLIJSON(command.OutOrStdout(), map[string]any{"recipes": recipes})
		}
		if len(recipes) == 0 {
			_, err = fmt.Fprintln(command.OutOrStdout(), "No recipes. Create one with pb recipe create -f <file>.")
			return err
		}
		writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 3, ' ', 0)
		fmt.Fprintln(writer, "NAME\tTAGS\tMACHINES\tEXPIRES\tLAST USED")
		for _, r := range recipes {
			var parsed recipe.Recipe
			_ = json.Unmarshal(r.Recipe, &parsed)
			machines := fmt.Sprint(r.Machines)
			if r.MaxMachines > 0 {
				machines += fmt.Sprintf(" / %d", r.MaxMachines)
			}
			used := "never"
			if r.LastUsedAt != nil {
				used = relativeTime(r.LastUsedAt)
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", r.Name, firstNonEmpty(strings.Join(parsed.Tags, ","), "-"), machines, r.ExpiresAt.Local().Format("2006-01-02"), used)
		}
		return writer.Flush()
	}}
	command.Flags().Bool("json", false, "print JSON")
	return command
}

func recipeShowCommand() *cobra.Command {
	command := &cobra.Command{Use: "show <name>", Short: "Show a recipe, its file and its machines", Long: `Show one recipe: its revision, public token id, expiry and creator, the file as it
was created or last updated (comments kept), and every machine paired from it with
whether it is online, which revision it applied and any section that failed to
apply.`, Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		client, workspace, err := policyClient(command, args)
		if err != nil {
			return err
		}
		r, machines, err := client.GetRecipe(command.Context(), workspace, args[0])
		if err != nil {
			return friendlyCommandError(err)
		}
		if wantsJSON(command) {
			return writeCLIJSON(command.OutOrStdout(), map[string]any{"recipe": r, "machines": machines})
		}
		out := command.OutOrStdout()
		fmt.Fprintf(out, "Recipe %s, revision %d, token %s, expires %s, created by %s\n", r.Name, r.Revision, r.Token, r.ExpiresAt.Local().Format("2006-01-02"), strings.TrimPrefix(r.CreatedBy, "user:"))
		fmt.Fprint(out, strings.TrimRight(r.Source, "\n")+"\n")
		of := ""
		if r.MaxMachines > 0 {
			of = fmt.Sprintf(" of %d", r.MaxMachines)
		}
		fmt.Fprintf(out, "Machines (%d%s)\n", len(machines), of)
		writer := tabwriter.NewWriter(out, 0, 4, 3, ' ', 0)
		for _, m := range machines {
			fmt.Fprintf(writer, "  %s\t%s\t%s\n", m.Name, map[bool]string{true: "online", false: "offline"}[m.Online], recipeMachineState(m))
		}
		return writer.Flush()
	}}
	command.Flags().Bool("json", false, "print JSON")
	return command
}

// recipeMachineState is one machine's line in show: a failed section, removal, or the applied revision.
func recipeMachineState(m api.RecipeMachine) string {
	var state map[string]struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(m.ApplyState, &state)
	sections := make([]string, 0, len(state))
	for section := range state {
		sections = append(sections, section)
	}
	sort.Strings(sections)
	for _, section := range sections {
		if e := state[section].Error; e != "" {
			return section + " failed: " + e + " (pb doctor on the machine)"
		}
	}
	if m.RemoveAt != nil {
		return "removed " + relativeTime(m.RemoveAt)
	}
	if m.AppliedRevision == 0 {
		return "not applied yet"
	}
	return fmt.Sprintf("applied revision %d   %s", m.AppliedRevision, relativeTime(m.LastSeenAt))
}

func recipeUpdateCommand() *cobra.Command {
	command := &cobra.Command{Use: "update -f <file>", Short: "Change a recipe from its file", Long: `Change a recipe to what its file now says; the recipe is named by the file's base
name. The token stays the same. Online machines from the recipe apply the new
revision at once (tags change server-side; ENV, config, preview and tunnel sections
are created, changed or removed on each machine).`, Example: "  pb recipe update -f ci.toml", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
		path, err := recipeFileFlag(command)
		if err != nil {
			return err
		}
		name, format, source, parsed, err := readRecipeFile(path)
		if err != nil {
			return err
		}
		client, workspace, err := policyClient(command, args)
		if err != nil {
			return err
		}
		var wraps []api.RecipeVaultWrap
		if vaults := parsed.Vaults(); len(vaults) > 0 {
			current, _, err := client.GetRecipe(command.Context(), workspace, name)
			if err != nil {
				return friendlyCommandError(err)
			}
			if len(current.PublicKey) != 32 {
				return fmt.Errorf("recipe %s has no public key to wrap vault keys to; rotate it (pb recipe rotate %s) and update again", name, name)
			}
			if wraps, err = recipeVaultWraps(command, name, vaults, current.PublicKey, recipeTokenID(current.Token)); err != nil {
				return err
			}
		}
		updated, err := client.UpdateRecipe(command.Context(), workspace, name, api.RecipeUpdate{Source: source, Format: format, VaultWraps: wraps})
		if err != nil {
			return friendlyCommandError(err)
		}
		if wantsJSON(command) {
			return writeCLIJSON(command.OutOrStdout(), updated)
		}
		changed := recipeChangedSections(updated.Previous.Recipe, updated.Recipe.Recipe)
		if updated.Recipe.Revision == updated.Previous.Revision || len(changed) == 0 {
			_, err = fmt.Fprintf(command.OutOrStdout(), "Recipe %s is unchanged (revision %d).\n", name, updated.Recipe.Revision)
			return err
		}
		_, err = fmt.Fprintf(command.OutOrStdout(), "Recipe %s updated to revision %d: %s changed. %d online machine%s will apply it now.\n", name, updated.Recipe.Revision, strings.Join(changed, ", "), updated.OnlineMachines, plural(updated.OnlineMachines))
		return err
	}}
	command.Flags().StringP("file", "f", "", "the recipe file (.toml or .json)")
	command.Flags().Bool("json", false, "print JSON")
	return command
}

// recipeChangedSections names the top-level keys whose canonical value differs.
func recipeChangedSections(before, after json.RawMessage) []string {
	var a, b map[string]json.RawMessage
	_ = json.Unmarshal(before, &a)
	_ = json.Unmarshal(after, &b)
	var out []string
	for key := range a {
		if _, ok := b[key]; !ok {
			out = append(out, key)
		}
	}
	for key, value := range b {
		if !bytes.Equal(a[key], value) {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

func recipeRotateCommand() *cobra.Command {
	command := &cobra.Command{Use: "rotate <name>", Short: "Give a recipe a new token; its machines keep working", Long: `Give a recipe a new token. The old token stops working at once; machines already
paired from it keep running. Without --confirm, print what changes and the
confirmation code; run it again with --confirm <code>. The new token is printed once:
replace PB_RECIPE everywhere the old one is stored.`, Example: "  pb recipe rotate ci\n  pb recipe rotate ci --confirm K3M9QX", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		client, workspace, err := policyClient(command, args)
		if err != nil {
			return err
		}
		currentRecipe, machines, err := client.GetRecipe(command.Context(), workspace, args[0])
		if err != nil {
			return friendlyCommandError(err)
		}
		id := recipeTokenID(currentRecipe.Token)
		token, err := recipe.NewToken(id)
		if err != nil || !recipe.ValidID(id) {
			return fmt.Errorf("the server returned recipe token %q, which has no valid id", currentRecipe.Token)
		}
		confirm, _ := command.Flags().GetString("confirm")
		// The recipe's own vault wraps move to the new key; no vault gets a new key.
		var current recipe.Recipe
		if err := json.Unmarshal(currentRecipe.Recipe, &current); err != nil {
			return fmt.Errorf("read recipe %s: %w", args[0], err)
		}
		var wraps []api.RecipeVaultWrap
		// Each running machine gets the new key sealed to its own key, so it keeps its vaults.
		var sealed []api.RecipeMachineSeal
		unsealed := 0
		if confirm != "" {
			if wraps, err = recipeVaultWraps(command, args[0], current.Vaults(), token.PublicKey(), token.ID); err != nil {
				return err
			}
			for _, m := range machines {
				if len(m.PublicKey) != 32 {
					unsealed++
					continue
				}
				box, err := recipe.SealRecipeKey(m.PublicKey, token.ID, token.PrivateKey())
				if err != nil {
					return fmt.Errorf("seal the new recipe key for %s: %w", m.Name, err)
				}
				sealed = append(sealed, api.RecipeMachineSeal{MachineID: m.ID, Sealed: box})
			}
		}
		rotated, err := client.RotateRecipe(command.Context(), workspace, args[0], api.RecipeRotate{Auth: token.Auth(), PublicKey: token.PublicKey(), VaultWraps: wraps, MachineKeys: sealed, Confirm: confirm})
		if details, ok := api.RecipeConfirmationDetails(err); ok {
			if wantsJSON(command) {
				return writeCLIJSON(command.OutOrStdout(), map[string]any{"state": "confirmation_required", "confirm": details.Confirm, "impact": details.Impact})
			}
			out := command.OutOrStdout()
			fmt.Fprintf(out, "Rotating %s gives it a new token.\n", args[0])
			fmt.Fprintf(out, "  The old token stops working at once. Its %d machine%s will keep running.\n", len(details.Impact.Machines), plural(len(details.Impact.Machines)))
			fmt.Fprintf(out, "  Then replace PB_RECIPE everywhere the old token is stored.\nRotate with: pb recipe rotate %s --confirm %s\n", args[0], details.Confirm)
			return nil
		}
		if err != nil {
			return friendlyCommandError(err)
		}
		if wantsJSON(command) {
			return writeCLIJSON(command.OutOrStdout(), map[string]any{"recipe": rotated, "token": token.String()})
		}
		if _, err = fmt.Fprintf(command.OutOrStdout(), "Recipe %s rotated. New token (shown once):\n  %s\n", rotated.Name, token); err != nil {
			return err
		}
		if unsealed > 0 {
			_, err = fmt.Fprintf(command.OutOrStdout(), "%d machine%s did not report a key yet; run the recipe install there again to keep reading its vaults.\n", unsealed, plural(unsealed))
		}
		return err
	}}
	command.Flags().String("confirm", "", "the confirmation code printed without --confirm")
	command.Flags().Bool("json", false, "print JSON (the new token included, once)")
	return command
}

func recipeRevokeCommand() *cobra.Command {
	command := &cobra.Command{Use: "revoke <name>", Short: "Revoke a recipe and remove its machines", Long: `Revoke a recipe: its token stops working and every machine paired from it is
removed. Without --confirm, print what changes and the confirmation code; run it
again with --confirm <code>. Use pb recipe rotate instead to replace a leaked token
and keep the machines.`, Example: "  pb recipe revoke ci\n  pb recipe revoke ci --confirm 7RT2WD", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(command *cobra.Command, args []string) error {
		client, workspace, err := policyClient(command, args)
		if err != nil {
			return err
		}
		confirm, _ := command.Flags().GetString("confirm")
		revoked, err := client.RevokeRecipe(command.Context(), workspace, args[0], confirm)
		if details, ok := api.RecipeConfirmationDetails(err); ok {
			if wantsJSON(command) {
				return writeCLIJSON(command.OutOrStdout(), map[string]any{"state": "confirmation_required", "confirm": details.Confirm, "impact": details.Impact})
			}
			out := command.OutOrStdout()
			fmt.Fprintf(out, "Revoking %s:\n  - its token stops working\n", args[0])
			names := make([]string, 0, len(details.Impact.Machines))
			for _, m := range details.Impact.Machines {
				names = append(names, m.Name)
			}
			if len(names) > 0 {
				fmt.Fprintf(out, "  - its %d machine%s will be removed: %s\n", len(names), plural(len(names)), strings.Join(names, ", "))
			}
			fmt.Fprintf(out, "Revoke with: pb recipe revoke %s --confirm %s\n", args[0], details.Confirm)
			return nil
		}
		if err != nil {
			return friendlyCommandError(err)
		}
		if wantsJSON(command) {
			return writeCLIJSON(command.OutOrStdout(), revoked)
		}
		out := command.OutOrStdout()
		fmt.Fprintf(out, "Recipe %s revoked. %d machine%s removed.\n", revoked.Recipe.Name, len(revoked.Impact.Machines), plural(len(revoked.Impact.Machines)))
		if revoked.RemovalError != "" {
			fmt.Fprintln(out, revoked.RemovalError)
		}
		return nil
	}}
	command.Flags().String("confirm", "", "the confirmation code printed without --confirm")
	command.Flags().Bool("json", false, "print JSON")
	return command
}

// recipeTokenID is the public id in a masked token ("pbr_<id>_…").
func recipeTokenID(masked string) string {
	return strings.TrimSuffix(strings.TrimPrefix(masked, recipe.TokenPrefix), "_…")
}
