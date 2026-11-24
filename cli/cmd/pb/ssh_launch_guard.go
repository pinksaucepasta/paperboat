package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/pinksaucepasta/paperboat/cli/internal/managedssh"
)

// verifyManagedSSHLaunch must pass before any OpenSSH process is started for a Paperboat machine: without Paperboat's
// transport and host keys OpenSSH would connect to whatever answers the machine's name. A configuration that is
// missing or damaged is rewritten by the daemon once.
func verifyManagedSSHLaunch(ctx context.Context, destination managedssh.Destination, executable string, arguments, environment []string) error {
	edges := hostDependenciesOf(ctx)
	paths, err := edges.daemonPaths()
	if err != nil {
		return commandPreparationFailure{step: prepareSSHConfiguration, cause: err}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return commandPreparationFailure{step: prepareSSHConfiguration, cause: err}
	}
	agentSocket, err := managedssh.InstalledAgentSocket(paths.RuntimeRoot)
	if err != nil {
		return commandPreparationFailure{step: prepareSSHConfiguration, cause: err}
	}
	launch := managedssh.OpenSSHLaunch{
		Home: home, OwnerUID: uint32(os.Geteuid()), AgentSocket: agentSocket, Domain: destination.AliasDomain,
		Host: destination.Host, Executable: executable, Arguments: arguments, Environment: environment,
	}
	err = edges.verifySSHLaunch(ctx, launch)
	if err != nil && ctx.Err() == nil && !errors.Is(err, managedssh.ErrOpenSSHLaunchUnpinned) && !errors.Is(err, managedssh.ErrOpenSSHUnavailable) {
		if refreshErr := edges.refreshInventory(ctx); refreshErr != nil {
			err = errors.Join(err, refreshErr)
		} else {
			err = edges.verifySSHLaunch(ctx, launch)
		}
	}
	var unsafePath *managedssh.UnsafeSSHPathError
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.As(err, &unsafePath):
		return unsafePath // its own message names the folder and the fix
	case errors.Is(err, managedssh.ErrOpenSSHLaunchUnpinned):
		return commandPreparationFailure{step: prepareSSHLaunchOptions, cause: err}
	case errors.Is(err, managedssh.ErrOpenSSHUnavailable):
		return commandPreparationFailure{step: prepareOpenSSH, cause: err}
	}
	return commandPreparationFailure{step: prepareSSHConfiguration, cause: err}
}

// verifyManagedToolLaunch checks the ssh that scp, sftp or rsync will start, with the ssh options in their arguments.
func verifyManagedToolLaunch(ctx context.Context, tool string, args []string, destination managedssh.Destination, user string, environment []string) error {
	executable, options, err := managedToolSSH(tool, args, environment)
	if err != nil {
		return err
	}
	if tool == "rsync" {
		if runtime.GOOS == "windows" { // executeManagedSSHTool gives rsync's ssh the same HOME
			home, err := os.UserHomeDir()
			if err != nil {
				return commandPreparationFailure{step: prepareSSHConfiguration, cause: err}
			}
			environment = withEnvironment(environment, "HOME", home)
		}
		// targetRsyncPath starts plain ssh whatever -e says.
		if len(options) > 0 || executable != "ssh" {
			if err := verifyManagedSSHLaunch(ctx, destination, "ssh", []string{user + "@" + destination.Host}, environment); err != nil {
				return err
			}
		}
	}
	return verifyManagedSSHLaunch(ctx, destination, executable, append(options, user+"@"+destination.Host), environment)
}

// managedToolSSH is the ssh program a tool starts and the ssh options its own arguments give that program.
func managedToolSSH(tool string, args, environment []string) (executable string, options []string, err error) {
	refused := commandPreparationFailure{step: prepareSSHLaunchOptions, cause: managedssh.ErrOpenSSHLaunchUnpinned}
	if tool == "rsync" {
		shell, chosen := rsyncRemoteShell(args, environment)
		if !chosen {
			return "ssh", nil, nil
		}
		// shortcut: a quoted word in -e is refused rather than parsed the way rsync does, parse it if people need one.
		words := strings.Fields(shell)
		if len(words) == 0 || strings.ContainsAny(shell, "\"'") {
			return "", nil, refused
		}
		if name := strings.TrimSuffix(strings.ToLower(filepath.Base(words[0])), ".exe"); name != "ssh" {
			return "", nil, refused
		}
		path, err := exec.LookPath(words[0])
		if err != nil {
			return "", nil, refused
		}
		return path, words[1:], nil
	}
	valueOptions := "cDFiJloPSX"
	if tool == "sftp" {
		valueOptions = "BbcDFiJloPRSsX"
	}
	for index := 0; index < len(args); index++ {
		value := args[index]
		if value == "--" {
			break
		}
		if !strings.HasPrefix(value, "-") {
			continue
		}
		for at := 1; at < len(value); at++ {
			if !strings.ContainsRune(valueOptions, rune(value[at])) {
				continue
			}
			argument := value[at+1:]
			if argument == "" {
				if index++; index >= len(args) {
					return "", nil, refused
				}
				argument = args[index]
			}
			switch value[at] {
			case 'S': // another program in place of ssh
				return "", nil, refused
			case 'F', 'J', 'o':
				options = append(options, "-"+string(value[at]), argument)
			}
			break
		}
	}
	// The tool starts the ssh installed with it.
	if path, err := exec.LookPath(tool); err == nil {
		name := "ssh"
		if runtime.GOOS == "windows" {
			name = "ssh.exe"
		}
		if path, err = filepath.Abs(path); err == nil {
			if sibling := filepath.Join(filepath.Dir(path), name); fileExists(sibling) {
				return sibling, options, nil
			}
		}
	}
	return "ssh", options, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
