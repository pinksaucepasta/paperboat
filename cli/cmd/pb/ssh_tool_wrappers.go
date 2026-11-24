package main

import (
	"errors"
	"os"
	"strings"

	"github.com/pinksaucepasta/paperboat/cli/internal/command"
	"github.com/pinksaucepasta/paperboat/cli/internal/managedssh"
	"github.com/spf13/cobra"
)

// errManagedToolOperand never carries the operand: it is untrusted text (terminal escapes, private paths).
var errManagedToolOperand = errors.New("managed SSH tool operand is invalid")

func managedToolOperandError(message string) error {
	return usageError{err: errManagedToolOperand, publicMessage: message}
}

// machineOperandError shows the fixed reason a [user@]machine operand was refused.
func machineOperandError(err error) error {
	for _, reason := range []error{managedssh.ErrMachineReferenceInvalid, managedssh.ErrSSHUsernameInvalid, managedssh.ErrSSHAliasInvalid} {
		if errors.Is(err, reason) {
			return usageError{err: err, publicMessage: reason.Error()}
		}
	}
	return invocationError(err)
}

// managedToolHelp reports a request for pb's help. rsync's -h means human-readable sizes, so -h counts only alone.
func managedToolHelp(args []string) bool {
	return len(args) > 0 && (args[0] == "--help" || len(args) == 1 && args[0] == "-h")
}

type managedToolTargetResolver func(*command.Context, string, string) (managedssh.Destination, error)

// remoteDrivePathConverter turns a drive-letter path on a Windows target into the form its rsync reads as absolute.
type remoteDrivePathConverter func(destination managedssh.Destination, user, path string) (string, error)

func newManagedSSHToolCommand(name string) *cobra.Command {
	command := &cobra.Command{
		Use:                name + " <standard " + name + " arguments...>",
		Short:              "Run " + name + " with Paperboat machine resolution",
		Args:               commandArgs(cobra.MinimumNArgs(1)),
		DisableFlagParsing: true,
		RunE: func(c *cobra.Command, args []string) error {
			if managedToolHelp(args) {
				return c.Help()
			}
			return actionManagedSSHTool(c, name, args)
		},
	}
	return command
}

func actionManagedSSHTool(cobraCommand *cobra.Command, tool string, args []string) error {
	ctx := actionContext(cobraCommand, nil)
	domain, err := configuredManagedSSHDomain(ctx)
	if err != nil {
		return err
	}
	resolve := func(ctx *command.Context, target, requestedUser string) (managedssh.Destination, error) {
		_, machine, sshTarget, err := resolveSSHCommandTargetWarm(ctx, target)
		if err != nil {
			return managedssh.Destination{}, friendlyCommandError(err)
		}
		if err := requireManagedSSHConfiguration(); err != nil {
			return managedssh.Destination{}, err
		}
		// The daemon already knows when managed SSH cannot work (for example the account's client-key limit):
		// say so, instead of letting OpenSSH fail with "Permission denied (publickey)".
		if err := sshTargetHealth(ctx.Context, machine.ID, machine.Alias); err != nil {
			return managedssh.Destination{}, err
		}
		return managedssh.ResolveDestination(managedssh.DestinationInput{
			Alias: machine.Alias, AliasDomain: domain,
			RegisteredPort: sshTarget.Port, RequestedUser: requestedUser, RegisteredUser: sshTarget.OSUser,
			HasRegisteredUser: true, Platform: machine.Platform,
		})
	}
	environment, err := managedSSHEnvironment(ctx, os.Environ())
	if err != nil {
		return err
	}
	rewritten, err := rewriteManagedToolArguments(ctx, tool, args, domain, resolve, func(destination managedssh.Destination, user, path string) (string, error) {
		return targetRsyncPath(ctx.Context, environment, destination, user, path)
	})
	if err != nil {
		return err
	}
	return executeManagedSSHTool(cobraCommand.Context(), tool, rewritten, environment)
}

func rewriteManagedToolArguments(ctx *command.Context, tool string, args []string, domain string, resolve managedToolTargetResolver, drivePath remoteDrivePathConverter) ([]string, error) {
	if ctx == nil || resolve == nil || len(args) == 0 {
		return nil, errManagedToolOperand
	}
	result := append([]string(nil), args...)
	switch tool {
	case "scp", "rsync":
		managed := 0
		for index, value := range result {
			prefix, path, ok := splitManagedRemoteOperand(value)
			if !ok {
				continue
			}
			// Only an operand naming a machine (name, alias or machine ID) is managed; any other host:path
			// passes to the tool unchanged.
			target, requestedUser, err := managedssh.ParseMachineReference(prefix, domain)
			if err != nil {
				continue
			}
			destination, err := resolve(ctx, target, requestedUser)
			if err != nil {
				return nil, err
			}
			user := requestedUser
			if user == "" {
				user = destination.User
			}
			if tool == "rsync" && drivePath != nil && strings.EqualFold(destination.Platform, "windows") && isDriveOperand(path) {
				if path, err = drivePath(destination, user, path); err != nil {
					return nil, err
				}
			}
			result[index] = user + "@" + destination.Host + ":" + path
			managed++
		}
		if managed == 0 {
			return nil, managedToolOperandError(tool + " requires a remote [user@]machine:path operand; machine must be a machine name or machine ID (machine_<id>)")
		}
	case "sftp":
		index := sftpDestination(result)
		if index < 0 {
			return nil, managedToolOperandError("sftp requires [user@]machine")
		}
		operand, suffix := result[index], ""
		if prefix, path, ok := splitManagedRemoteOperand(operand); ok {
			operand, suffix = prefix, ":"+path
		}
		target, requestedUser, err := managedssh.ParseMachineReference(operand, domain)
		if err != nil {
			return nil, machineOperandError(err)
		}
		destination, err := resolve(ctx, target, requestedUser)
		if err != nil {
			return nil, err
		}
		user := requestedUser
		if user == "" {
			user = destination.User
		}
		result[index] = user + "@" + destination.Host + suffix
	default:
		return nil, errManagedToolOperand
	}
	return result, nil
}

func splitManagedRemoteOperand(value string) (prefix, path string, ok bool) {
	if value == "" || strings.HasPrefix(value, "-") || strings.Contains(value, "://") {
		return "", "", false
	}
	colon := strings.IndexByte(value, ':')
	if colon <= 0 || colon == 1 && len(value) > 2 && (value[2] == '\\' || value[2] == '/') {
		return "", "", false
	}
	return value[:colon], value[colon+1:], true
}

func sftpDestination(args []string) int {
	// OpenSSH options whose separate form consumes the following argv entry.
	valueOptions := map[string]bool{"-B": true, "-b": true, "-c": true, "-D": true, "-F": true, "-i": true, "-J": true, "-l": true, "-o": true, "-P": true, "-R": true, "-S": true, "-s": true, "-X": true}
	for index := 0; index < len(args); index++ {
		value := args[index]
		if value == "--" {
			if index+1 < len(args) {
				return index + 1
			}
			return -1
		}
		if valueOptions[value] {
			index++
			continue
		}
		if strings.HasPrefix(value, "-") {
			continue
		}
		// SFTP accepts a remote destination followed by an optional local path.
		return index
	}
	return -1
}
