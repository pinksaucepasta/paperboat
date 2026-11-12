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

// envHookPowerShell is the one Windows shell with a hook. cmd.exe has none: its
// quoting cannot carry arbitrary values (`"`, `%`, `&` and newlines break out of
// set "NAME=value"), conda and clink only use AutoRun for fixed code, never for
// secrets; `pb env run -- cmd` hands values to the process without a parser.
const envHookPowerShell = "powershell"

// envHookShellsHere are the shells the hook supports on this OS.
func envHookShellsHere() []string {
	if runtime.GOOS == "windows" {
		return []string{envHookPowerShell}
	}
	return envHookShells
}

func addEnvironmentHookCommand(root *cobra.Command) {
	hook := &cobra.Command{
		Use:   "hook [bash|zsh|fish|powershell]",
		Short: "Load ENV in plain ssh and VS Code shells through a shell startup hook",
		Long: "Without flags, print the hook line for a shell. --install adds that line to the shell's startup file " +
			"(~/.bashrc, ~/.zshenv, ~/.config/fish/config.fish or, on Windows, the PowerShell profile.ps1 of Windows PowerShell and PowerShell 7; cmd.exe has no hook, use `pb env run -- <command>`) for the account logged in here and the active workspace, and defines a pb shell function so `pb env load` loads ENV into the current shell; " +
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
	shell := envHookPowerShell
	if len(args) == 1 {
		shell = args[0]
	} else if runtime.GOOS != "windows" {
		shell = filepath.Base(os.Getenv("SHELL"))
	}
	for _, known := range envHookShellsHere() {
		if shell == known {
			return shell, nil
		}
	}
	if runtime.GOOS == "windows" {
		return "", localArgumentError("only PowerShell has a hook: pb env hook powershell. cmd.exe cannot load values safely; run `pb env run -- <command>` there")
	}
	return "", localArgumentError("name the shell: pb env hook bash, zsh or fish")
}

// envHookLine is the whole hook. It and its pb function run pb by absolute path
// because ssh shells often lack pb on PATH (macOS: ~/.local/bin), and does nothing when pb
// is gone. It also defines a pb function (the nvm/conda way) that handles
// only `pb env load`, loading into the current shell, and passes every other
// call to the real pb unchanged.
func envHookLine(shell, pb string) string {
	if shell == envHookPowerShell {
		return powerShellHookLine(pb)
	}
	quoted := shellSingleQuote(pb)
	if shell == "fish" {
		return fmt.Sprintf("test -x %s; and %s env hook --emit fish 2>/dev/null | source; "+
			"function pb; if test (count $argv) -eq 2; and test \"$argv[1]\" = env; and test \"$argv[2]\" = load; "+
			"%s env hook --emit fish | source; else; %s $argv; end; end %s", quoted, quoted, quoted, quoted, envHookMarker)
	}
	return fmt.Sprintf("[ -x %s ] && eval \"$(%s env hook --emit %s 2>/dev/null)\"; "+
		"pb() { if [ $# -eq 2 ] && [ \"$1\" = env ] && [ \"$2\" = load ]; then eval \"$(%s env hook --emit %s)\"; "+
		"else %s \"$@\"; fi; } %s", quoted, quoted, shell, quoted, shell, quoted, envHookMarker)
}

// powerShellHookLine is envHookLine for PowerShell: same absolute path, silent
// when pb is gone or fails, and a pb function for `pb env load` that otherwise
// forwards arguments, piped input and the exit code to the real pb. The
// values reach the shell only as single-quoted literals.
func powerShellHookLine(pb string) string {
	q := powerShellSingleQuote(pb)
	load := "& { $s = (& " + q + " env hook --emit powershell | Out-String); if ($s) { Invoke-Expression $s } }"
	quiet := strings.Replace(load, "powershell |", "powershell 2>$null |", 1)
	return "if (Test-Path -LiteralPath " + q + ") { try { " + quiet + " } catch {} }; " +
		"function pb { if ($args.Count -eq 2 -and $args[0] -eq 'env' -and $args[1] -eq 'load') { " + load + " } " +
		"elseif ($MyInvocation.ExpectingInput) { $input | & " + q + " @args } else { & " + q + " @args } } " + envHookMarker
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

// envHookRCFiles are the startup files a shell reads; PowerShell has one per
// edition (CurrentUserAllHosts of Windows PowerShell 5.1 and PowerShell 7).
func envHookRCFiles(home, shell string) []string {
	if shell == envHookPowerShell {
		docs := envHookDocumentsDir(home)
		return []string{filepath.Join(docs, "WindowsPowerShell", "profile.ps1"), filepath.Join(docs, "PowerShell", "profile.ps1")}
	}
	return []string{envHookRCFile(home, shell)}
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
	files := envHookRCFiles(home, shell)
	state := "already installed in"
	for _, rc := range files {
		changed, err := writeEnvHookLine(rc, envHookLine(shell, envHookExecutable()), shell == "bash")
		if err != nil {
			return fmt.Errorf("update %s: %w", rc, err)
		}
		if changed {
			state = "installed in"
		}
	}
	_, err = fmt.Fprintf(command.OutOrStdout(), "Paperboat ENV hook %s %s for workspace %s.\nNew ssh shells load it; VS Code Remote-SSH: run \"Remote-SSH: Kill VS Code Server on Host\" and reconnect.\n", state, strings.Join(files, ", "), workspace)
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
	eol := "\n"
	if strings.Contains(string(data), "\r\n") { // a CRLF file (a Windows profile) keeps its line endings
		eol = "\r\n"
	}
	if found == 1 && strings.Contains(string(data), line+eol) {
		return false, nil
	}
	var next string
	switch {
	case kept == "" && strings.HasSuffix(path, ".ps1"):
		// Windows PowerShell 5.1 reads a profile without a BOM as ANSI, which garbles a non-ASCII user path.
		next = "\ufeff" + line + "\n"
	case first:
		next = line + eol + kept
	case kept == "" || strings.HasSuffix(kept, "\n"):
		next = kept + line + eol
	default:
		next = kept + eol + line + eol
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

func envHookAllRCFiles(home string) []string {
	var files []string
	for _, shell := range envHookShellsHere() {
		files = append(files, envHookRCFiles(home, shell)...)
	}
	return files
}

func uninstallEnvironmentHook(command *cobra.Command) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	removed := []string{}
	for _, rc := range envHookAllRCFiles(home) {
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
	for _, rc := range envHookAllRCFiles(home) {
		data, err := os.ReadFile(rc)
		if err == nil {
			if _, found := removeEnvHookLines(string(data)); found > 0 {
				installed = append(installed, rc)
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
