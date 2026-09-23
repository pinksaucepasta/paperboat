package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
	"github.com/pinksaucepasta/paperboat/internal/environmentmanager"
	"github.com/pinksaucepasta/paperboat/internal/prompt"
	"github.com/spf13/cobra"
)

var passwordVaultForCommand = func(command *cobra.Command) (environmentmanager.PasswordVault, error) {
	client, store, profile, err := e2eeClient(actionContext(command, nil))
	if err != nil {
		return environmentmanager.PasswordVault{}, err
	}
	if err := store.RequireEnvironmentSecureStore(); err != nil {
		return environmentmanager.PasswordVault{}, err
	}
	return environmentmanager.PasswordVault{Client: client, Store: store, Issuer: profile.Issuer, AccountID: profile.Account.ID}, nil
}

var passwordVaultPrompt = func(command *cobra.Command, title string) ([]byte, error) {
	input, ok := command.InOrStdin().(*os.File)
	if !ok || !environmentVariableTerminal(command) {
		return nil, errors.New("vault passwords require an interactive hidden prompt")
	}
	return prompt.Secret(prompt.SecretOptions{Context: command.Context(), Title: title, Stdin: input, Output: command.ErrOrStderr(), MaxBytes: 1024})
}

func addPasswordVaultCommands(root *cobra.Command) {
	vault := &cobra.Command{Use: "vault", Short: "Manage password-protected ENV key custody", Args: commandArgs(cobra.NoArgs)}
	for _, action := range []string{"init", "unlock", "password", "lock", "resume", "recovery", "recover"} {
		cmd := &cobra.Command{Use: action, Args: commandArgs(cobra.NoArgs)}
		switch action {
		case "init":
			cmd.Short = "Create a password-protected ENV vault"
		case "unlock":
			cmd.Short = "Unlock this account's ENV vault on this device"
		case "password":
			cmd.Short = "Rewrap unlocked ENV keys with a new password"
		case "lock":
			cmd.Short = "Clear unlocked vault keys while retaining encrypted custody"
		case "recovery":
			cmd.Short = "Enable, replace, or disable the optional recovery code"
		case "recover":
			cmd.Short = "Recover vault access and replace the password and recovery code"
		case "resume":
			cmd.Short = "Reconcile an interrupted vault publication"
		}

		if action == "init" || action == "recovery" || action == "recover" {
			cmd.Flags().String("recovery-file", "", "save a newly generated recovery code to a new absolute file path")
		}
		if action == "recovery" {
			cmd.Flags().Bool("disable", false, "disable recovery-code access to the current vault")
		}
		if action == "recover" {
			cmd.Flags().String("recovery-input-file", "", "read the old recovery code from an absolute owner-only file")
		}
		if action == "init" || action == "unlock" || action == "password" || action == "recover" {
			addVaultPasswordFlags(cmd)
		}
		addVaultJSONFlag(cmd)
		cmd.RunE = func(command *cobra.Command, _ []string) error {
			recoverySaved := false
			var password, oldCode []byte
			var err error
			if action == "init" || action == "unlock" || action == "password" {
				password, err = vaultPasswordForOperation(command, "Master password", action != "unlock")
				if err != nil {
					return err
				}
				defer clear(password)
			}
			if action == "recover" {
				path, pathErr := command.Flags().GetString("recovery-file")
				if pathErr != nil || strings.TrimSpace(path) == "" {
					return errors.New("recovery requires --recovery-file for the replacement code")
				}
				oldCode, err = readVaultRecoveryInput(command)
				if err != nil {
					return err
				}
				defer clear(oldCode)
				password, err = vaultPasswordForOperation(command, "New master password", true)
				if err != nil {
					return err
				}
				defer clear(password)
			}
			if action == "recovery" {
				disabled, flagErr := command.Flags().GetBool("disable")
				path, pathErr := command.Flags().GetString("recovery-file")
				if flagErr != nil || pathErr != nil || disabled && strings.TrimSpace(path) != "" || !disabled && strings.TrimSpace(path) == "" {
					return errors.New("choose --disable or --recovery-file")
				}
			}
			manager, err := passwordVaultForCommand(command)
			if err != nil {
				return err
			}
			switch action {
			case "lock":
				err = manager.Store.LockPasswordVault(manager.Issuer, manager.AccountID)
			case "resume":
				err = manager.Resume(command.Context())
			case "recovery":
				disabled, _ := command.Flags().GetBool("disable")
				path, _ := command.Flags().GetString("recovery-file")
				var code []byte
				if !disabled {
					code, err = saveNewVaultRecoveryCode(path)
					if err != nil {
						return err
					}
					defer clear(code)
					recoverySaved = true
				}
				err = manager.ReplaceRecovery(command.Context(), code)
			case "recover":
				path, _ := command.Flags().GetString("recovery-file")
				replacement, saveErr := saveNewVaultRecoveryCode(path)
				if saveErr != nil {
					return saveErr
				}
				defer clear(replacement)
				recoverySaved = true
				err = manager.Recover(command.Context(), oldCode, password, replacement)
			default:
				switch action {
				case "init":
					path, _ := command.Flags().GetString("recovery-file")
					var code []byte
					if path != "" {
						code, err = saveNewVaultRecoveryCode(path)
						if err != nil {
							return err
						}
						defer clear(code)
						recoverySaved = true
					}
					err = manager.InitializeWithRecovery(command.Context(), password, code)
				case "unlock":
					err = manager.Unlock(command.Context(), password)
				case "password":
					err = manager.ChangePassword(command.Context(), password)
				}
			}
			if err != nil {
				if recoverySaved {
					return fmt.Errorf("%w; retain the new recovery file: activation is unconfirmed; run `pb env vault resume` to reconcile any pending publication before retrying", err)
				}
				return err
			}
			fields := map[string]any{}
			switch action {
			case "lock":
				fields["locked"] = true
			case "resume":
				fields["reconciled"] = true
			case "recovery":
				disabled, _ := command.Flags().GetBool("disable")
				path, _ := command.Flags().GetString("recovery-file")
				fields["recovery_enabled"] = !disabled
				if !disabled {
					fields["recovery_file"] = path
				}
			case "recover":
				path, _ := command.Flags().GetString("recovery-file")
				fields["recovery_file"] = path
				fields["recovered"] = true
			default:
				switch action {
				case "init":
					fields["initialized"] = true
					path, _ := command.Flags().GetString("recovery-file")
					if strings.TrimSpace(path) != "" {
						fields["recovery_file"] = path
					}
				case "unlock":
					fields["unlocked"] = true
				case "password":
					fields["password_updated"] = true
				}
			}
			return writeVaultResult(command, action, "ENV vault operation completed.", fields)
		}
		vault.AddCommand(cmd)
	}
	remove := &cobra.Command{
		Use:   "remove",
		Short: "Remove this device's local ENV vault custody",
		Args:  commandArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			return runPasswordVaultRemove(command)
		},
	}
	remove.Flags().String("confirm", "", "exact confirmation phrase: REMOVE LOCAL ENV <account_id>")
	addVaultJSONFlag(remove)
	vault.AddCommand(remove)
	reset := &cobra.Command{
		Use:   "reset",
		Short: "Replace personal ENV keys and delete every personal value",
		Args:  commandArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			return runPasswordVaultReset(command)
		},
	}
	reset.Flags().String("confirm", "", "exact confirmation phrase: RESET ENV <account_id>")
	reset.Flags().String("recovery-file", "", "save a newly generated recovery code to a new absolute file path")
	addVaultPasswordFlags(reset)
	addVaultJSONFlag(reset)
	vault.AddCommand(reset)
	root.AddCommand(vault)
}

func runPasswordVaultReset(command *cobra.Command) error {
	passwordPath, passwordStdin, err := vaultPasswordSource(command)
	if err != nil {
		return err
	}
	if jsonOutputRequested(command) && passwordPath == "" && !passwordStdin {
		return errors.New("--json requires --password-file or --password-stdin")
	}
	manager, err := passwordVaultForCommand(command)
	if err != nil {
		return err
	}
	confirmation, _ := command.Flags().GetString("confirm")
	if confirmation != "RESET ENV "+manager.AccountID {
		return invocationError(errors.New("--confirm must exactly match `RESET ENV <account_id>`"))
	}
	password, explicit, err := readVaultPasswordSource(command)
	if err != nil {
		return err
	}
	if explicit {
		if len(password) == 0 {
			clear(password)
			return errors.New("master password cannot be empty")
		}
		defer clear(password)
	} else {
		password, err = confirmedVaultPassword(command)
		if err != nil {
			return err
		}
		defer clear(password)
	}
	recoveryPath, _ := command.Flags().GetString("recovery-file")
	var recovery []byte
	recoverySaved := false
	if recoveryPath != "" {
		recovery, err = saveNewVaultRecoveryCode(recoveryPath)
		if err != nil {
			return err
		}
		defer clear(recovery)
		recoverySaved = true
	}
	err = manager.ResetPersonal(command.Context(), password, recovery, true)
	if err != nil {
		if recoverySaved {
			return fmt.Errorf("%w; retain the new recovery file: activation is unconfirmed; run `pb env vault resume` to reconcile any pending publication before retrying", err)
		}
		return safeEnvironmentVariableCommandError(err)
	}
	fields := map[string]any{"reset": true}
	if recoveryPath != "" {
		fields["recovery_file"] = recoveryPath
	}
	return writeVaultResult(command, "reset", "ENV personal vault reset completed.", fields)
}

func runPasswordVaultRemove(command *cobra.Command) error {
	manager, err := passwordVaultForCommand(command)
	if err != nil {
		return err
	}
	confirmation, _ := command.Flags().GetString("confirm")
	if confirmation != "REMOVE LOCAL ENV "+manager.AccountID {
		return invocationError(errors.New("vault remove requires --confirm `REMOVE LOCAL ENV <account_id>`"))
	}
	if err := manager.Store.RemovePasswordVault(manager.Issuer, manager.AccountID); err != nil {
		return safeEnvironmentVariableCommandError(err)
	}
	return writeVaultResult(command, "remove", "Removed local ENV vault custody; cloud personal and team ciphertext was not changed.", map[string]any{"removed": true, "remote_unchanged": true})
}

func saveNewVaultRecoveryCode(path string) ([]byte, error) {
	code, err := environmente2ee.GenerateVaultRecoveryCode()
	if err != nil {
		return nil, err
	}
	if err := writeEnvironmentRecoveryFile(path, code); err != nil {
		clear(code)
		return nil, err
	}
	return code, nil
}
func confirmedVaultPassword(command *cobra.Command) ([]byte, error) {
	return vaultPasswordForOperation(command, "New master password", true)
}
