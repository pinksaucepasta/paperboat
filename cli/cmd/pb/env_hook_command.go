package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/spf13/cobra"
)

// envHookMarker ends the one line `pb env hook --install` owns in a shell rc
// file; --uninstall removes exactly the lines that end with it.
const envHookMarker = "# paperboat-env-hook"

const envHookSchema = "paperboat.env-hook/v1"

// envHookRecord is the account and workspace chosen at install time. It holds
// no values and no credentials.
type envHookRecord struct {
	Schema      string `json:"schema"`
	ServerURL   string `json:"server_url"`
	AccountID   string `json:"account_id"`
	WorkspaceID string `json:"workspace_id"`
}

var envHookShells = []string{"bash", "zsh", "fish"}

func addEnvironmentHookCommand(root *cobra.Command) {
	hook := &cobra.Command{
		Use:   "hook [bash|zsh|fish]",
		Short: "Load ENV in plain ssh and VS Code shells through a shell startup hook",
		Long: "Without flags, print the hook line for a shell. --install adds that line to the shell's startup file " +
			"(~/.bashrc, ~/.zshenv or ~/.config/fish/config.fish) for the account logged in here and the active workspace; " +
			"--uninstall removes it. VS Code Remote-SSH: after installing, run \"Remote-SSH: Kill VS Code Server on Host\" and reconnect.",
		Args: commandArgs(cobra.MaximumNArgs(1)),
		RunE: runEnvironmentHook,
		// --emit runs at every shell start: it skips the root pre-run, whose
		// update-service check alone costs ~180 ms and may print notices.
		PersistentPreRunE: func(command *cobra.Command, args []string) error {
			if shell, _ := command.Flags().GetString("emit"); shell != "" {
				return nil
			}
			return command.Root().PersistentPreRunE(command, args)
		},
	}
	hook.Flags().Bool("install", false, "add the hook line to the shell startup file and record the account and workspace")
	hook.Flags().Bool("uninstall", false, "remove the hook line from every shell startup file and forget the recorded choice")
	hook.Flags().Bool("status", false, "show whether the hook is installed and whose ENV it loads")
	hook.Flags().String("emit", "", "")
	_ = hook.Flags().MarkHidden("emit")
	root.AddCommand(hook)
}

func runEnvironmentHook(command *cobra.Command, args []string) error {
	if shell, _ := command.Flags().GetString("emit"); shell != "" {
		emitEnvironmentHook(command, shell)
		return nil
	}
	if runtime.GOOS == "windows" {
		return localArgumentError("pb env hook supports bash, zsh and fish on Linux and macOS")
	}
	install, _ := command.Flags().GetBool("install")
	uninstall, _ := command.Flags().GetBool("uninstall")
	status, _ := command.Flags().GetBool("status")
	if install && uninstall || status && (install || uninstall) {
		return localArgumentError("use only one of --install, --uninstall and --status")
	}
	if uninstall {
		return uninstallEnvironmentHook(command)
	}
	if status {
		return environmentHookStatus(command)
	}
	shell, err := envHookShell(args)
	if err != nil {
		return err
	}
	if install {
		return installEnvironmentHook(command, shell)
	}
	_, err = fmt.Fprintln(command.OutOrStdout(), envHookLine(shell, envHookExecutable()))
	return err
}

func envHookShell(args []string) (string, error) {
	shell := ""
	if len(args) == 1 {
		shell = args[0]
	} else {
		shell = filepath.Base(os.Getenv("SHELL"))
	}
	for _, known := range envHookShells {
		if shell == known {
			return shell, nil
		}
	}
	return "", localArgumentError("name the shell: pb env hook bash, zsh or fish")
}

// envHookLine is the whole hook. It runs pb by absolute path because
// non-interactive ssh shells often lack pb on PATH, and does nothing when pb
// is gone.
func envHookLine(shell, pb string) string {
	quoted := shellSingleQuote(pb)
	if shell == "fish" {
		return fmt.Sprintf("test -x %s; and %s env hook --emit fish 2>/dev/null | source %s", quoted, quoted, envHookMarker)
	}
	return fmt.Sprintf("[ -x %s ] && eval \"$(%s env hook --emit %s 2>/dev/null)\" %s", quoted, quoted, shell, envHookMarker)
}

// envHookExecutable prefers the stable pb on PATH over this process's
// versioned release path, which an update may remove.
func envHookExecutable() string {
	if path, err := exec.LookPath("pb"); err == nil {
		if absolute, err := filepath.Abs(path); err == nil {
			return absolute
		}
	}
	if path, err := os.Executable(); err == nil {
		return path
	}
	return "pb"
}

func envHookRCFile(home, shell string) string {
	switch shell {
	case "zsh":
		return filepath.Join(home, ".zshenv")
	case "fish":
		return filepath.Join(home, ".config", "fish", "config.fish")
	}
	return filepath.Join(home, ".bashrc")
}

func envHookRecordPath(command *cobra.Command) (string, error) {
	path := configPathFlag(command)
	if path == "" {
		var err error
		if path, err = config.DefaultPath(); err != nil {
			return "", err
		}
	}
	return filepath.Join(filepath.Dir(path), "env-hook.json"), nil
}

func installEnvironmentHook(command *cobra.Command, shell string) error {
	cfg, err := workspaceConfig(configPathFlag(command), workspaceServerOverride(command))
	if err != nil {
		return err
	}
	account, err := workspaceAccountID(cfg)
	if err != nil && !errors.Is(err, config.ErrNoCredentials) {
		return fmt.Errorf("read the logged-in account: %w", err)
	}
	if account == "" {
		return localArgumentError("log in first with `pb login`; the hook loads the ENV of the account logged in for this user")
	}
	workspace, err := effectiveWorkspace(command)
	if err != nil {
		return err
	}
	recordPath, err := envHookRecordPath(command)
	if err != nil {
		return err
	}
	body, err := json.Marshal(envHookRecord{Schema: envHookSchema, ServerURL: cfg.ServerURL, AccountID: account, WorkspaceID: workspace})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(recordPath), 0o700); err != nil {
		return err
	}
	if err := atomicfile.Write(recordPath, body, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1}); err != nil {
		return fmt.Errorf("record the hook's account and workspace: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	rc := envHookRCFile(home, shell)
	changed, err := writeEnvHookLine(rc, envHookLine(shell, envHookExecutable()), shell == "bash")
	if err != nil {
		return fmt.Errorf("update %s: %w", rc, err)
	}
	state := "already installed in"
	if changed {
		state = "installed in"
	}
	_, err = fmt.Fprintf(command.OutOrStdout(), "Paperboat ENV hook %s %s for workspace %s.\nNew ssh shells load it; VS Code Remote-SSH: run \"Remote-SSH: Kill VS Code Server on Host\" and reconnect.\n", state, rc, workspace)
	return err
}

// writeEnvHookLine puts line in path once. bash reads ~/.bashrc in
// non-interactive ssh commands but most distributions return early from it
// for them, so the bash line goes first. An existing hook line is replaced in
// place; reports whether the file changed.
func writeEnvHookLine(path, line string, first bool) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	mode := fs.FileMode(0o644)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	}
	kept, found := removeEnvHookLines(string(data))
	if found == 1 && strings.Contains(string(data), line+"\n") {
		return false, nil
	}
	var next string
	switch {
	case first:
		next = line + "\n" + kept
	case kept == "" || strings.HasSuffix(kept, "\n"):
		next = kept + line + "\n"
	default:
		next = kept + "\n" + line + "\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, atomicfile.Write(path, []byte(next), atomicfile.Options{Mode: mode, OwnerUID: -1, OwnerGID: -1})
}

func removeEnvHookLines(content string) (string, int) {
	lines := strings.SplitAfter(content, "\n")
	kept := make([]string, 0, len(lines))
	found := 0
	for _, line := range lines {
		if strings.HasSuffix(strings.TrimRight(line, "\r\n"), envHookMarker) {
			found++
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, ""), found
}

func uninstallEnvironmentHook(command *cobra.Command) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	removed := []string{}
	for _, shell := range envHookShells {
		rc := envHookRCFile(home, shell)
		data, err := os.ReadFile(rc)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", rc, err)
		}
		kept, found := removeEnvHookLines(string(data))
		if found == 0 {
			continue
		}
		info, err := os.Stat(rc)
		if err != nil {
			return err
		}
		if err := atomicfile.Write(rc, []byte(kept), atomicfile.Options{Mode: info.Mode().Perm(), OwnerUID: -1, OwnerGID: -1}); err != nil {
			return fmt.Errorf("update %s: %w", rc, err)
		}
		removed = append(removed, rc)
	}
	recordPath, err := envHookRecordPath(command)
	if err != nil {
		return err
	}
	if err := os.Remove(recordPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", recordPath, err)
	}
	if len(removed) == 0 {
		_, err = fmt.Fprintln(command.OutOrStdout(), "Paperboat ENV hook was not installed; nothing changed.")
		return err
	}
	_, err = fmt.Fprintf(command.OutOrStdout(), "Paperboat ENV hook removed from %s. Open a new shell to drop the values.\n", strings.Join(removed, ", "))
	return err
}

func environmentHookStatus(command *cobra.Command) error {
	out := command.OutOrStdout()
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	installed := []string{}
	for _, shell := range envHookShells {
		data, err := os.ReadFile(envHookRCFile(home, shell))
		if err == nil {
			if _, found := removeEnvHookLines(string(data)); found > 0 {
				installed = append(installed, envHookRCFile(home, shell))
			}
		}
	}
	if len(installed) == 0 {
		_, err = fmt.Fprintln(out, "Paperboat ENV hook: not installed. Run `pb env hook --install`.")
		return err
	}
	record, recordErr := readEnvHookRecord(command)
	if recordErr != nil {
		_, err = fmt.Fprintf(out, "Paperboat ENV hook: in %s but no recorded account; run `pb env hook --install` again.\n", strings.Join(installed, ", "))
		return err
	}
	result := loadEnvironmentHook(command.Context(), command, record)
	_, err = fmt.Fprintf(out, "Paperboat ENV hook: in %s, workspace %s. %s\n", strings.Join(installed, ", "), record.WorkspaceID, result.summary())
	return err
}
