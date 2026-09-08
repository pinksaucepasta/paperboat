package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/spf13/cobra"
)

var errManagedToolOperand = errors.New("managed SSH tool operand is invalid")

type managedToolTargetResolver func(*command.Context, string, string) (managedssh.Destination, error)

func newManagedSSHToolCommand(name string) *cobra.Command {
	command := &cobra.Command{
		Use:                name + " <standard " + name + " arguments...>",
		Short:              "Run " + name + " with Paperboat machine resolution",
		Args:               commandArgs(cobra.MinimumNArgs(1)),
		DisableFlagParsing: true,
		RunE: func(c *cobra.Command, args []string) error {
			return actionManagedSSHTool(c, name, args)
		},
	}
	return command
}

func actionManagedSSHTool(cobraCommand *cobra.Command, tool string, args []string) error {
	ctx := actionContext(cobraCommand, args)
	resolve := func(ctx *command.Context, target, requestedUser string) (managedssh.Destination, error) {
		_, machine, sshTarget, err := resolveSSHCommandTargetFast(ctx, target)
		if err != nil {
			return managedssh.Destination{}, friendlyCommandError(err)
		}
		return managedssh.ResolveDestination(managedssh.DestinationInput{
			Alias: machine.Alias, AliasSuffix: managedssh.AliasSuffix,
			RegisteredPort: sshTarget.Port, RequestedUser: requestedUser, RegisteredUser: sshTarget.OSUser,
			HasRegisteredUser: true, Platform: machine.Platform,
		})
	}
	rewritten, err := rewriteManagedToolArguments(ctx, tool, args, resolve)
	if err != nil {
		return err
	}
	return executeManagedSSHTool(tool, rewritten, os.Environ())
}

func rewriteManagedToolArguments(ctx *command.Context, tool string, args []string, resolve managedToolTargetResolver) ([]string, error) {
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
			target, requestedUser, err := managedssh.ParseMachineTarget(prefix)
			if err != nil {
				continue
			}
			target = strings.TrimSuffix(strings.ToLower(target), "."+managedssh.AliasSuffix)
			if _, err := managedssh.AliasHost(strings.ToLower(target), managedssh.AliasSuffix); err != nil {
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
			result[index] = user + "@" + destination.Host + ":" + path
			managed++
		}
		if managed == 0 {
			return nil, fmt.Errorf("%w: %s requires a remote machine:path operand", errManagedToolOperand, tool)
		}
	case "sftp":
		index := lastSFTPDestination(result)
		if index < 0 {
			return nil, fmt.Errorf("%w: sftp requires [user@]machine", errManagedToolOperand)
		}
		target, requestedUser, err := managedssh.ParseMachineTarget(result[index])
		if err != nil {
			return nil, err
		}
		target = strings.TrimSuffix(strings.ToLower(target), "."+managedssh.AliasSuffix)
		destination, err := resolve(ctx, target, requestedUser)
		if err != nil {
			return nil, err
		}
		user := requestedUser
		if user == "" {
			user = destination.User
		}
		result[index] = user + "@" + destination.Host
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

func lastSFTPDestination(args []string) int {
	// OpenSSH options whose separate form consumes the following argv entry.
	valueOptions := map[string]bool{"-B": true, "-b": true, "-c": true, "-D": true, "-F": true, "-i": true, "-J": true, "-l": true, "-o": true, "-P": true, "-R": true, "-S": true, "-s": true, "-X": true}
	last := -1
	for index := 0; index < len(args); index++ {
		value := args[index]
		if value == "--" {
			if index+1 < len(args) {
				last = index + 1
			}
			break
		}
		if valueOptions[value] {
			index++
			continue
		}
		if strings.HasPrefix(value, "-") {
			continue
		}
		last = index
	}
	return last
}
