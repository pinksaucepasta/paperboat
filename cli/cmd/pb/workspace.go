package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	sessionauth "github.com/pinksaucepasta/paperboat/internal/auth"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

const workspacePreferenceVersion = 1

type workspaceInvocationKey struct{}

type workspacePreferenceFile struct {
	Version   int    `json:"version"`
	Workspace string `json:"workspace"`
}

type workspaceEnvironmentFile struct {
	Version   int    `json:"version"`
	MachineID string `json:"machine_id"`
}

var workspaceTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
var chooseWorkspace = selector.Choose

func workspaceSwitchCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "switch [workspace]",
		Short: "Switch the active Personal or team workspace",
		Long:  "Choose an available workspace for resource discovery and new operations. Running operations and this machine's enrollment remain unchanged. Use Personal for personally owned resources or a team slug for team resources; switching never changes the machine selected for enrollment. The selected workspace is saved for this account and server. --workspace overrides PAPERBOAT_WORKSPACE, which overrides the saved default; without either, Personal is active.",
		Args:  commandArgs(cobra.MaximumNArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {
			return actionSwitchWorkspace(command, args)
		},
	}
}

func actionSwitchWorkspace(command *cobra.Command, args []string) error {
	_, err := selectWorkspace(command, args)
	return err
}

func selectWorkspace(command *cobra.Command, args []string) (bool, error) {
	ctx := actionContext(command, args)
	cfg, store, profile, err := e2eeWorkspaceIdentity(ctx)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(profile.Account.ID) == "" {
		return false, errors.New("the signed-in account has no stable ID; sign in again before switching workspaces")
	}
	source := (&sessionauth.Source{Store: store, Issuer: cfg.ServerURL}).WithContext(ctx.Context)
	credential, err := source.Credential()
	if err != nil {
		if errors.Is(err, config.ErrNoCredentials) || errors.Is(err, config.ErrSecretNotFound) {
			return false, errors.New("not signed in to Paperboat; run `pb login` before switching workspaces")
		}
		return false, err
	}
	client := api.New(cfg.ServerURL, credential, nil)
	page, err := client.ListWorkspaces(ctx.Context)
	if err != nil {
		return false, friendlyCommandError(err)
	}
	workspaces := page.Items
	if len(args) == 1 {
		err := saveSelectedWorkspace(command, cfg, profile.Account.ID, args[0], workspaces)
		return err == nil, err
	}
	selectedWorkspace, err := effectiveWorkspace(command)
	if err != nil {
		return false, err
	}
	if !workspaceTerminal() {
		return false, errors.New("choose a workspace in an interactive terminal or run `pb switch <workspace>`")
	}
	items := make([]selector.Item, 0, len(workspaces))
	for _, workspace := range workspaces {
		description := workspace.Kind
		if workspace.Role != "" {
			description += " · " + workspace.Role
		}
		if workspace.ID == selectedWorkspace {
			description += " · current"
		}
		items = append(items, selector.Item{ID: workspace.ID, Title: workspace.Name, Description: description, Search: workspace.ID + " " + workspace.Name})
	}
	selection, err := chooseWorkspace(selector.Options{
		Context: command.Context(), Title: "Switch workspace", Subtitle: "Resource discovery and new operations use this workspace",
		Items: items, Empty: "No workspaces are available", Footer: "↑/↓ move  enter switch  esc cancel",
		Stdin: os.Stdin, Output: command.ErrOrStderr(),
	})
	if err != nil {
		if errors.Is(err, selector.ErrCanceled) {
			return false, nil
		}
		return false, err
	}
	err = saveSelectedWorkspace(command, cfg, profile.Account.ID, selection.ID, workspaces)
	return err == nil, err
}

func saveSelectedWorkspace(command *cobra.Command, cfg *config.Config, accountID, selector string, available []api.Workspace) error {
	selector = strings.TrimSpace(selector)
	if err := api.ValidateWorkspaceSelector(selector); err != nil {
		return invocationError(err)
	}
	var selected *api.Workspace
	for index := range available {
		if available[index].ID == selector {
			selected = &available[index]
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("workspace %q is not available to this account; choose a listed workspace or run `pb switch personal`", selector)
	}
	if err := saveWorkspaceDefault(cfg, cfg.ServerURL, accountID, selector); err != nil {
		return fmt.Errorf("save active workspace: %w", err)
	}
	if strings.TrimSpace(os.Getenv("PAPERBOAT_WORKSPACE")) == "" {
		_, err := fmt.Fprintf(command.OutOrStdout(), "Switched active workspace to %s. New commands use this scope; running operations are unchanged.\n", selected.Name)
		return err
	}
	_, err := fmt.Fprintf(command.OutOrStdout(), "Saved %s as the default workspace. PAPERBOAT_WORKSPACE overrides it for commands that set that variable.\n", selected.Name)
	return err
}

func captureWorkspaceInvocation(command *cobra.Command, args []string) error {
	if workspaceIndependentCommand(command) {
		return nil
	}
	// A direct switch is a recovery path even if the previously saved selector
	// is malformed or has been revoked. Its argument is the target, so do not
	// let an old default or PAPERBOAT_WORKSPACE prevent selecting Personal.
	if command.Name() == "switch" && len(args) == 1 {
		selected, err := validateWorkspaceSelection(args[0], "switch target")
		if err != nil {
			return err
		}
		command.SetContext(context.WithValue(command.Context(), workspaceInvocationKey{}, selected))
		return nil
	}
	selected, err := resolveWorkspaceInvocation(command)
	if err != nil {
		return err
	}
	command.SetContext(context.WithValue(command.Context(), workspaceInvocationKey{}, selected))
	return nil
}

func workspaceIndependentCommand(command *cobra.Command) bool {
	if command == nil {
		return false
	}
	parts := strings.Fields(command.CommandPath())
	if len(parts) < 2 {
		return false
	}
	switch parts[1] {
	case "login", "auth", "daemon", "doctor", "fresh", "install", "manual-install", "pair", "relay", "service", "setup", "uninstall", "update":
		return true
	case "machine":
		return len(parts) > 2 && parts[2] == "add"
	default:
		return false
	}
}

func resolveWorkspaceInvocation(command *cobra.Command) (string, error) {
	root := command.Root()
	if root != nil {
		if flag := root.PersistentFlags().Lookup("workspace"); flag != nil && flag.Changed {
			return validateWorkspaceSelection(flag.Value.String(), "--workspace")
		}
	}
	if value := strings.TrimSpace(os.Getenv("PAPERBOAT_WORKSPACE")); value != "" {
		return validateWorkspaceSelection(value, "PAPERBOAT_WORKSPACE")
	}
	cfg, err := workspaceConfig(configPathFlag(command), workspaceServerOverride(command))
	if err != nil {
		return "", err
	}
	accountID, err := workspaceAccountID(cfg)
	if err != nil {
		if errors.Is(err, config.ErrNoCredentials) {
			return "personal", nil
		}
		return "", fmt.Errorf("read active workspace identity: %w", err)
	}
	if accountID == "" {
		return "personal", nil
	}
	selected, err := loadWorkspaceDefault(cfg, cfg.ServerURL, accountID)
	if err != nil {
		return "", fmt.Errorf("read active workspace: %w", err)
	}
	if selected == "" {
		selected = "personal"
	}
	return validateWorkspaceSelection(selected, "saved workspace")
}

func effectiveWorkspace(command *cobra.Command) (string, error) {
	if selected, ok := command.Context().Value(workspaceInvocationKey{}).(string); ok && selected != "" {
		return selected, nil
	}
	if root := command.Root(); root != nil {
		if flag := root.PersistentFlags().Lookup("workspace"); flag != nil && flag.Changed {
			return validateWorkspaceSelection(flag.Value.String(), "--workspace")
		}
	}
	if value := strings.TrimSpace(os.Getenv("PAPERBOAT_WORKSPACE")); value != "" {
		return validateWorkspaceSelection(value, "PAPERBOAT_WORKSPACE")
	}
	return resolveWorkspaceInvocation(command)
}

func validateWorkspaceSelection(value, source string) (string, error) {
	value = strings.TrimSpace(value)
	if err := api.ValidateWorkspaceSelector(value); err != nil {
		return "", invocationError(fmt.Errorf("invalid %s: %w", source, err))
	}
	return value, nil
}

func workspaceFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	selected, ok := ctx.Value(workspaceInvocationKey{}).(string)
	return selected, ok && selected != ""
}

func workspaceForCommandContext(commandContext *command.Context) (string, error) {
	if commandContext == nil {
		return "", errors.New("Paperboat command context is missing")
	}
	if selected, ok := workspaceFromContext(commandContext.Context); ok {
		return selected, nil
	}
	if selected := strings.TrimSpace(commandContext.String("workspace")); selected != "" {
		return validateWorkspaceSelection(selected, "--workspace")
	}
	if selected := strings.TrimSpace(os.Getenv("PAPERBOAT_WORKSPACE")); selected != "" {
		return validateWorkspaceSelection(selected, "PAPERBOAT_WORKSPACE")
	}
	cfg, err := workspaceConfig(commandContext.String("config"), commandContext.String("server"))
	if err != nil {
		return "", err
	}
	accountID, err := workspaceAccountID(cfg)
	if errors.Is(err, config.ErrNoCredentials) || accountID == "" && err == nil {
		return "personal", nil
	}
	if err != nil {
		return "", err
	}
	selected, err := loadWorkspaceDefault(cfg, cfg.ServerURL, accountID)
	if err != nil {
		return "", err
	}
	if selected == "" {
		selected = "personal"
	}
	return validateWorkspaceSelection(selected, "saved workspace")
}

func newWorkspaceAPIClient(commandContext *command.Context, server string, credential config.Credential) (*api.Client, error) {
	client := api.New(server, credential, nil)
	selected, err := workspaceForCommandContext(commandContext)
	if err != nil {
		return nil, err
	}
	if err := client.SetWorkspace(selected); err != nil {
		return nil, err
	}
	return client, nil
}

func workspaceConfig(configPath, serverOverride string) (*config.Config, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	if value := strings.TrimSpace(serverOverride); value != "" {
		cfg.ServerURL, err = config.NormalizeServerURL(value)
		if err != nil {
			return nil, err
		}
	} else if strings.TrimSpace(cfg.ServerURL) != "" {
		cfg.ServerURL, err = config.NormalizeServerURL(cfg.ServerURL)
		if err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

func workspaceServerOverride(command *cobra.Command) string {
	if root := command.Root(); root != nil {
		if flag := root.PersistentFlags().Lookup("server"); flag != nil {
			return flag.Value.String()
		}
	}
	return ""
}

func workspaceAccountID(cfg *config.Config) (string, error) {
	if cfg == nil || strings.TrimSpace(cfg.ServerURL) == "" {
		return "", nil
	}
	store, err := readOnlyWorkspaceProfileStore(cfg)
	if err != nil {
		return "", err
	}
	profile, err := store.Load(cfg.ServerURL)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(profile.Account.ID), nil
}

func readOnlyWorkspaceProfileStore(cfg *config.Config) (config.ProfileStore, error) {
	defaultDir, err := config.DefaultCredentialDir()
	if err != nil {
		return config.ProfileStore{}, err
	}
	dir := cfg.Auth.ProfileDir
	if dir == "" {
		dir = defaultDir
	} else if !filepath.IsAbs(dir) {
		return config.ProfileStore{}, errors.New("auth.profile_dir must be an absolute path")
	}
	return config.ProfileStore{Path: filepath.Clean(dir)}, nil
}

func workspacePreferencePath(cfg *config.Config, server, accountID, scope string) (string, error) {
	if cfg == nil || strings.TrimSpace(cfg.Path()) == "" || strings.TrimSpace(server) == "" || strings.TrimSpace(accountID) == "" {
		return "", errors.New("workspace preferences require a config path, server, and account ID")
	}
	configPath, err := filepath.Abs(cfg.Path())
	if err != nil {
		return "", err
	}
	key := sha256.Sum256([]byte(strings.TrimSpace(server) + "\x00" + strings.TrimSpace(accountID) + "\x00" + scope))
	return configPath + ".workspaces/" + hex.EncodeToString(key[:]) + ".json", nil
}

func loadWorkspaceDefault(cfg *config.Config, server, accountID string) (string, error) {
	path, err := workspacePreferencePath(cfg, server, accountID, "active")
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var stored workspacePreferenceFile
	if err := json.Unmarshal(data, &stored); err != nil {
		return "", fmt.Errorf("parse workspace preference: %w", err)
	}
	if stored.Version != workspacePreferenceVersion || stored.Workspace == "" {
		return "", errors.New("unsupported or empty workspace preference")
	}
	if err := api.ValidateWorkspaceSelector(stored.Workspace); err != nil {
		return "", fmt.Errorf("invalid saved workspace: %w", err)
	}
	return stored.Workspace, nil
}

func saveWorkspaceDefault(cfg *config.Config, server, accountID, selected string) error {
	path, err := workspacePreferencePath(cfg, server, accountID, "active")
	if err != nil {
		return err
	}
	if err := api.ValidateWorkspaceSelector(selected); err != nil {
		return err
	}
	return writeWorkspaceRecord(path, workspacePreferenceFile{Version: workspacePreferenceVersion, Workspace: selected})
}

func workspaceLastEnvironment(cfg *config.Config, server, accountID, selected string) (string, error) {
	path, err := workspacePreferencePath(cfg, server, accountID, "machine:"+selected)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var stored workspaceEnvironmentFile
	if err := json.Unmarshal(data, &stored); err != nil {
		return "", fmt.Errorf("parse last environment preference: %w", err)
	}
	if stored.Version != workspacePreferenceVersion {
		return "", errors.New("unsupported last environment preference")
	}
	return strings.TrimSpace(stored.MachineID), nil
}

func rememberWorkspaceEnvironment(commandContext *command.Context, cfg *config.Config, machineID string) error {
	selected, err := workspaceForCommandContext(commandContext)
	if err != nil {
		return err
	}
	accountID, err := workspaceAccountID(cfg)
	if err != nil {
		return err
	}
	if accountID == "" {
		return errors.New("cannot remember an environment without a stable account ID")
	}
	previous, err := workspaceLastEnvironment(cfg, cfg.ServerURL, accountID, selected)
	if err != nil {
		return err
	}
	if previous == strings.TrimSpace(machineID) {
		return nil
	}
	path, err := workspacePreferencePath(cfg, cfg.ServerURL, accountID, "machine:"+selected)
	if err != nil {
		return err
	}
	return writeWorkspaceRecord(path, workspaceEnvironmentFile{Version: workspacePreferenceVersion, MachineID: strings.TrimSpace(machineID)})
}

func writeWorkspaceRecord(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create workspace preference directory: %w", err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode workspace preference: %w", err)
	}
	data = append(data, '\n')
	if err := atomicfile.Write(path, data, atomicfile.CurrentOwnerOptions(0o600)); err != nil {
		return fmt.Errorf("write workspace preference: %w", err)
	}
	return nil
}

func e2eeWorkspaceIdentity(commandContext *command.Context) (*config.Config, config.ProfileStore, config.Profile, error) {
	cfg, err := workspaceConfig(commandContext.String("config"), commandContext.String("server"))
	if err != nil {
		return nil, config.ProfileStore{}, config.Profile{}, err
	}
	if cfg.ServerURL == "" {
		return nil, config.ProfileStore{}, config.Profile{}, errors.New("Paperboat server is not configured; set server_url or use --server")
	}
	store, err := config.ProfileStoreFor(cfg)
	if err != nil {
		return nil, config.ProfileStore{}, config.Profile{}, err
	}
	profile, err := store.Load(cfg.ServerURL)
	if errors.Is(err, config.ErrNoCredentials) {
		return nil, config.ProfileStore{}, config.Profile{}, errors.New("not signed in to Paperboat; run `pb login` before switching workspaces")
	}
	if err != nil {
		return nil, config.ProfileStore{}, config.Profile{}, err
	}
	return cfg, store, profile, nil
}
