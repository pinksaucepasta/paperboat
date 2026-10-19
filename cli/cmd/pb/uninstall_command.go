package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	helperconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/inbox"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/userpaths"
	"github.com/spf13/cobra"
)

func uninstallCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "uninstall",
		Short: "Completely remove Paperboat from this machine",
		Args:  commandArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			jsonOutput, _ := command.Flags().GetBool("json")
			hostname, err := os.Hostname()
			if err != nil || strings.TrimSpace(hostname) == "" {
				return commandPreparationFailure{step: prepareMachineName, cause: err}
			}
			first, _ := command.Flags().GetString("confirmation")
			second, _ := command.Flags().GetString("hostname")
			// Explicit confirmation flags replace the prompts; prompt only for
			// values the caller did not supply.
			promptFirst := !jsonOutput && !command.Flags().Changed("confirmation")
			promptSecond := !jsonOutput && !command.Flags().Changed("hostname")
			if promptFirst || promptSecond {
				reader := bufio.NewReader(command.InOrStdin())
				fmt.Fprintln(command.ErrOrStderr(), "This permanently removes Paperboat services, binaries, credentials, configuration, and runtime state. The Paperboat Inbox is preserved.")
				if promptFirst {
					fmt.Fprint(command.ErrOrStderr(), "Type UNINSTALL PAPERBOAT to continue: ")
					first, err = reader.ReadString('\n')
					if err != nil && !errors.Is(err, io.EOF) {
						return err
					}
				}
				if promptSecond {
					fmt.Fprintf(command.ErrOrStderr(), "Type this machine hostname (%s) to confirm: ", hostname)
					second, err = reader.ReadString('\n')
					if err != nil && !errors.Is(err, io.EOF) {
						return err
					}
				}
			}
			if strings.TrimSpace(first) != "UNINSTALL PAPERBOAT" {
				return invocationError(errors.New("uninstall confirmation did not match"))
			}
			if strings.TrimSpace(second) != hostname {
				return invocationError(errors.New("hostname confirmation did not match"))
			}
			jsonWriter := command.OutOrStdout()
			if jsonOutput {
				command.SetOut(io.Discard)
				command.SetErr(io.Discard)
			}
			preservedInboxes, inboxErr := uninstallInboxPaths(command)
			executable, executableErr := os.Executable()
			home, homeErr := os.UserHomeDir()
			productHandoffReady := !platformProductHandoffRequired()
			productStatusPath := ""
			daemonStopConfirmed := false
			runtimePurged := false
			cleanupErr := performUninstallCleanup([]uninstallCleanupStep{
				{name: "remove installed CLI manuals", run: func() error {
					if homeErr != nil {
						return homeErr
					}
					return removeInstalledManuals(home)
				}},
				{name: "resolve Paperboat Inbox preservation", run: func() error { return inboxErr }},
				{name: "stop local daemon", run: func() error {
					if executableErr != nil {
						return executableErr
					}
					serviceCtx, cancelService := context.WithTimeout(context.WithoutCancel(command.Context()), 30*time.Second)
					defer cancelService()
					err := localdaemon.RemoveCurrentUserService(serviceCtx, executable)
					if err == nil {
						daemonStopConfirmed = true
					}
					return err
				}},
				{name: "remove managed OpenSSH configuration", run: func() error {
					if homeErr != nil {
						return homeErr
					}
					_, err := managedssh.UninstallOpenSSHConfig(home, uint32(os.Geteuid()))
					return err
				}},
				{name: "remove managed SSH public identity", run: func() error {
					if homeErr != nil {
						return homeErr
					}
					return managedssh.UninstallManagedIdentityPublicKey(home, uint32(os.Geteuid()))
				}},
				{name: "remove system Paperboat runtime", run: func() error {
					err := purgePlatformRuntime(command)
					runtimePurged = err == nil
					return err
				}},
				{name: "remove installed Paperboat product", run: func() error {
					if !runtimePurged {
						return errors.New("system runtime removal was not confirmed; product removal was skipped")
					}
					if platformRequiresConfirmedDaemonStop() && !daemonStopConfirmed {
						return errors.New("local daemon ownership or termination was not confirmed; product removal was skipped")
					}
					if inboxErr != nil {
						return errors.New("Paperboat Inbox location could not be proven; product removal was skipped")
					}
					productCtx, cancelProduct := context.WithTimeout(context.WithoutCancel(command.Context()), 5*time.Minute)
					defer cancelProduct()
					statusPath, err := removePlatformProductInstallation(productCtx, command, preservedInboxes, command.OutOrStdout())
					if err == nil {
						productStatusPath = statusPath
						productHandoffReady = true
					}
					return err
				}},
				{name: "remove user Paperboat state", run: func() error {
					if inboxErr != nil {
						return errors.New("Paperboat Inbox location could not be proven; state removal was skipped")
					}
					if !productHandoffReady {
						return errors.New("Windows cleanup helper did not start; state removal was skipped so uninstall can be retried safely")
					}
					if platformProductHandoffRequired() {
						// Windows must wait for this executable and its diagnostic
						// handles to close. The protected helper owns user cleanup.
						return nil
					}
					return removeUserStateAfterSystemPurge(daemonStopConfirmed, runtimePurged, func() error {
						return purgeUserPaperboatState(command, preservedInboxes)
					})
				}},
			})
			if cleanupErr != nil {
				if !jsonOutput {
					fmt.Fprintln(command.OutOrStdout(), "Paperboat attempted every local removal step. The Paperboat Inbox was preserved.")
				}
				return uninstallCleanupError{err: cleanupErr}
			}
			if jsonOutput {
				result := map[string]any{"uninstalled": true, "hostname": hostname, "inbox_preserved": true}
				if platformProductHandoffRequired() {
					result["uninstalled"] = false
					result["state"] = "pending"
					result["completion_status_path"] = productStatusPath
				}
				return writeCLIJSON(jsonWriter, result)
			}
			fmt.Fprintln(command.OutOrStdout(), platformUninstallSuccessMessage())
			return nil
		},
		SilenceUsage: true, SilenceErrors: true,
	}
	command.Flags().String("state-root", "", "additional Paperboat runtime state directory to remove")
	command.Flags().String("confirmation", "", "exact confirmation phrase: UNINSTALL PAPERBOAT")
	command.Flags().String("hostname", "", "exact current hostname confirmation")
	command.Flags().Bool("json", false, "print JSON")
	return command
}

type uninstallCleanupStep struct {
	name string
	run  func() error
}

// performUninstallCleanup deliberately runs every step. Complete uninstall is
// a local recovery boundary: a stopped service, unavailable broker, or failed
// network-shaped cleanup must never strand later-owned files or service state.
func performUninstallCleanup(steps []uninstallCleanupStep) error {
	var result error
	for _, step := range steps {
		if step.run == nil {
			result = errors.Join(result, uninstallStepFailure{name: step.name, err: errors.New("cleanup is unavailable")})
			continue
		}
		if err := step.run(); err != nil {
			result = errors.Join(result, uninstallStepFailure{name: step.name, err: err})
		}
	}
	return result
}

// removeUserStateAfterSystemPurge keeps credentials and state while a service
// may still run or the root purge did not confirm removal. A retry can then
// still find the identity it needs. Reset applies the same gate.
func removeUserStateAfterSystemPurge(daemonStopped, runtimePurged bool, purgeState func() error) error {
	if !daemonStopped || !runtimePurged {
		return errors.New("Paperboat service termination or system runtime removal was not confirmed; user state was retained for a safe retry")
	}
	return purgeState()
}

func purgeUserPaperboatState(command *cobra.Command, preserved []string) error {
	paths, pathErr := userPaperboatRemovalPaths(command)
	result := errors.Join(config.PurgeCredentialStore(), pathErr)
	for _, path := range paths {
		result = errors.Join(result, removePathPreserving(path, preserved))
	}
	return result
}

func userPaperboatRemovalPaths(command *cobra.Command) ([]string, error) {
	var result error
	var paths []string
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".local", "bin", "pb"))
	} else {
		result = errors.Join(result, err)
	}
	configPath, _ := command.Flags().GetString("config")
	if configPath != "" {
		if cfg, err := config.Load(configPath); err == nil && filepath.IsAbs(cfg.Auth.ProfileDir) {
			paths = append(paths, cfg.Auth.ProfileDir)
		}
		paths = append(paths, configPath)
	} else if path, err := config.DefaultPath(); err == nil {
		if cfg, loadErr := config.Load(path); loadErr == nil && filepath.IsAbs(cfg.Auth.ProfileDir) {
			paths = append(paths, cfg.Auth.ProfileDir)
		}
		paths = append(paths, filepath.Dir(path))
	}
	if dir, err := config.DefaultCredentialDir(); err == nil {
		paths = append(paths, filepath.Dir(dir))
	}
	if path, err := userpaths.Cache("paperboat"); err == nil {
		paths = append(paths, path)
	}
	if path, err := userpaths.Config("paperboat"); err == nil {
		paths = append(paths, path)
	}
	if path, err := userpaths.State("paperboat"); err == nil {
		paths = append(paths, path)
	}
	if path, err := userpaths.Data("paperboat"); err == nil {
		paths = append(paths, path)
	}
	if root, err := helperconfig.DefaultStateRoot(os.Getenv); err == nil {
		paths = append(paths, root)
	}
	if root, err := command.Flags().GetString("state-root"); err == nil && root != "" {
		paths = append(paths, filepath.Clean(root))
	}
	validPaths := paths[:0]
	for _, path := range paths {
		if !filepath.IsAbs(path) || filesystemRoot(path) {
			result = errors.Join(result, errors.New("refusing unsafe Paperboat removal path"))
			continue
		}
		validPaths = append(validPaths, filepath.Clean(path))
	}
	return validPaths, result
}

func uninstallInboxPaths(command *cobra.Command) ([]string, error) {
	roots := make(map[string]struct{})
	if root, err := helperconfig.DefaultStateRoot(os.Getenv); err == nil {
		roots[filepath.Clean(root)] = struct{}{}
	}
	if root, err := command.Flags().GetString("state-root"); err == nil && strings.TrimSpace(root) != "" {
		absolute, absoluteErr := filepath.Abs(root)
		if absoluteErr != nil {
			return nil, absoluteErr
		}
		roots[filepath.Clean(absolute)] = struct{}{}
	}
	paths := make(map[string]struct{})
	if path, err := inbox.DefaultPath(); err == nil {
		paths[filepath.Clean(path)] = struct{}{}
	}
	for root := range roots {
		registrationPath := filepath.Join(root, "machine-registration.json")
		info, err := os.Lstat(registrationPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 8192 {
			return nil, commandPreparationFailure{step: prepareInboxPreservation, cause: err}
		}
		encoded, err := os.ReadFile(registrationPath)
		if err != nil {
			return nil, err
		}
		var registration struct {
			InboxPath string `json:"inbox_path"`
		}
		if err := json.Unmarshal(encoded, &registration); err != nil || !filepath.IsAbs(registration.InboxPath) || filepath.Clean(registration.InboxPath) != registration.InboxPath || filesystemRoot(registration.InboxPath) {
			return nil, commandPreparationFailure{step: prepareInboxPreservation, cause: err}
		}
		paths[registration.InboxPath] = struct{}{}
	}
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	return result, nil
}

func removePathPreserving(root string, preserved []string) error {
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) || filesystemRoot(root) {
		return errors.New("refusing unsafe Paperboat removal path")
	}
	for _, keep := range preserved {
		keep = filepath.Clean(keep)
		if pathWithinOrEqual(keep, root) {
			// The cleanup root is the Inbox or is nested inside it. Preserve the
			// complete Inbox tree, including user-created content unknown to us.
			return nil
		}
	}
	containsInbox := false
	for _, keep := range preserved {
		if pathWithinOrEqual(root, filepath.Clean(keep)) {
			containsInbox = true
			break
		}
	}
	if !containsInbox {
		return os.RemoveAll(root)
	}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	unsafeTraversal, traversalErr := unsafeCleanupPathTraversal(root, info)
	if traversalErr != nil {
		return traversalErr
	}
	if unsafeTraversal || !info.IsDir() {
		return errors.New("refusing to traverse an unsafe Paperboat cleanup path containing the Inbox")
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var result error
	for _, entry := range entries {
		child := filepath.Join(root, entry.Name())
		result = errors.Join(result, removePathPreserving(child, preserved))
	}
	return result
}

func pathWithinOrEqual(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && (relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)))
}

func filesystemRoot(path string) bool {
	path = filepath.Clean(path)
	return path == filepath.VolumeName(path)+string(os.PathSeparator)
}
