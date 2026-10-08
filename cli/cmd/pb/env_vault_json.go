package main

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

const vaultSecretMaximumBytes = 1024

func addVaultJSONFlag(command *cobra.Command) {
	command.Flags().Bool("json", false, "print canonical JSON")
}

func addVaultPasswordFlags(command *cobra.Command) {
	command.Flags().String("password-file", "", "read the raw master password from an absolute owner-only file")
	command.Flags().Bool("password-stdin", false, "read the raw master password from non-interactive stdin")
}

func vaultPasswordSource(command *cobra.Command) (path string, stdin bool, err error) {
	if flag := command.Flags().Lookup("password-file"); flag != nil {
		path, err = command.Flags().GetString("password-file")
		if err != nil {
			return "", false, err
		}
		path = strings.TrimSpace(path)
	}
	if flag := command.Flags().Lookup("password-stdin"); flag != nil {
		stdin, err = command.Flags().GetBool("password-stdin")
		if err != nil {
			return "", false, err
		}
	}
	if path != "" && stdin {
		return "", false, invocationError(errors.New("choose --password-file or --password-stdin, not both"))
	}
	return path, stdin, nil
}

// readVaultPasswordSource returns an explicitly supplied password and whether
// a source was selected. It deliberately preserves every byte, including
// whitespace and a trailing newline.
func readVaultPasswordSource(command *cobra.Command) ([]byte, bool, error) {
	path, stdin, err := vaultPasswordSource(command)
	if err != nil {
		return nil, false, err
	}
	if path != "" {
		if !filepath.IsAbs(path) {
			return nil, true, invocationError(errors.New("--password-file requires an absolute path"))
		}
		value, err := readOwnerOnlyFile(path, vaultSecretMaximumBytes)
		if err != nil {
			clear(value)
			return nil, true, errors.New("--password-file must be an owner-only regular file no larger than 1024 bytes")
		}
		return value, true, nil
	}
	if !stdin {
		return nil, false, nil
	}
	if environmentVariableTerminal(command) {
		return nil, true, errors.New("--password-stdin requires non-interactive stdin")
	}
	value, err := readBoundedVaultSecret(command.InOrStdin())
	if err != nil {
		clear(value)
		return nil, true, err
	}
	return value, true, nil
}

func readBoundedVaultSecret(input io.Reader) ([]byte, error) {
	if input == nil {
		return nil, errors.New("could not read vault password")
	}
	value, err := io.ReadAll(io.LimitReader(input, vaultSecretMaximumBytes+1))
	if err != nil {
		clear(value)
		return nil, errors.New("could not read vault password")
	}
	if len(value) > vaultSecretMaximumBytes {
		clear(value)
		return nil, errors.New("vault password exceeds 1024 bytes")
	}
	return value, nil
}

// vaultPasswordForOperation gets a password for an operation that may need a
// confirmation. Explicit file/stdin input is already deliberate and is read
// once; interactive input retains the existing two-prompt confirmation.
func vaultPasswordForOperation(command *cobra.Command, title string, confirm bool) ([]byte, error) {
	password, explicit, err := readVaultPasswordSource(command)
	if err != nil {
		return nil, err
	}
	if !explicit {
		if jsonOutputRequested(command) {
			return nil, errors.New("--json requires --password-file or --password-stdin")
		}
		password, err = passwordVaultPrompt(command, title)
		if err != nil {
			return nil, err
		}
	}
	if len(password) == 0 {
		clear(password)
		return nil, errors.New("master password cannot be empty")
	}
	if !confirm || explicit {
		return password, nil
	}
	confirmation, err := passwordVaultPrompt(command, "Confirm "+strings.ToLower(title))
	if err != nil {
		clear(password)
		return nil, err
	}
	match := bytes.Equal(password, confirmation)
	clear(confirmation)
	if !match {
		clear(password)
		return nil, errors.New("master passwords do not match; vault unchanged")
	}
	return password, nil
}

func readVaultRecoveryInput(command *cobra.Command) ([]byte, error) {
	path := ""
	if flag := command.Flags().Lookup("recovery-input-file"); flag != nil {
		var err error
		path, err = command.Flags().GetString("recovery-input-file")
		if err != nil {
			return nil, err
		}
		path = strings.TrimSpace(path)
	}
	if path == "" {
		if jsonOutputRequested(command) {
			return nil, errors.New("--json recover requires --recovery-input-file")
		}
		return passwordVaultPrompt(command, "Recovery code")
	}
	if !filepath.IsAbs(path) {
		return nil, invocationError(errors.New("--recovery-input-file requires an absolute path"))
	}
	value, err := readOwnerOnlyFile(path, vaultSecretMaximumBytes)
	if err != nil {
		clear(value)
		return nil, errors.New("--recovery-input-file must be an owner-only regular file no larger than 1024 bytes")
	}
	// Recovery output files are newline terminated. Normalize that file
	// framing while keeping password input byte-for-byte unchanged.
	trimmed := bytes.TrimSpace(value)
	normalized := bytes.Clone(trimmed)
	clear(value)
	if len(normalized) == 0 {
		clear(normalized)
		return nil, errors.New("--recovery-input-file cannot be empty")
	}
	return normalized, nil
}

func writeVaultJSONResult(command *cobra.Command, action string, fields map[string]any) error {
	if fields == nil {
		fields = make(map[string]any)
	}
	fields["action"] = action
	fields["completed"] = true
	return writeCLIJSON(command.OutOrStdout(), fields)
}

func writeVaultResult(command *cobra.Command, action, message string, fields map[string]any) error {
	if jsonOutputRequested(command) {
		return writeVaultJSONResult(command, action, fields)
	}
	_, err := io.WriteString(command.OutOrStdout(), message+"\n")
	return err
}
