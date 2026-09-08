package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"

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
	return prompt.Secret(prompt.SecretOptions{Title: title, Stdin: input, Output: command.ErrOrStderr(), MaxBytes: 1024})
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
		cmd.RunE = func(command *cobra.Command, _ []string) error {
			recoverySaved := false
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
				if disabled && path != "" || !disabled && path == "" {
					return errors.New("choose --disable or --recovery-file")
				}
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
				if path == "" {
					return errors.New("recovery requires --recovery-file for the replacement code")
				}
				oldCode, promptErr := passwordVaultPrompt(command, "Recovery code")
				if promptErr != nil {
					return promptErr
				}
				defer clear(oldCode)
				password, promptErr := confirmedVaultPassword(command)
				if promptErr != nil {
					return promptErr
				}
				defer clear(password)
				replacement, saveErr := saveNewVaultRecoveryCode(path)
				if saveErr != nil {
					return saveErr
				}
				defer clear(replacement)
				recoverySaved = true
				err = manager.Recover(command.Context(), oldCode, password, replacement)
			default:
				password, promptErr := passwordVaultPrompt(command, "Master password")
				if promptErr != nil {
					return promptErr
				}
				defer clear(password)
				if len(password) == 0 {
					return errors.New("master password cannot be empty")
				}
				if action != "unlock" {
					confirmation, promptErr := passwordVaultPrompt(command, "Confirm master password")
					if promptErr != nil {
						return promptErr
					}
					match := bytes.Equal(password, confirmation)
					clear(confirmation)
					if !match {
						return errors.New("master passwords do not match; vault unchanged")
					}
				}
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
			_, err = fmt.Fprintln(command.OutOrStdout(), "ENV vault operation completed.")
			return err
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
	vault.AddCommand(reset)
	root.AddCommand(vault)
}

func runPasswordVaultReset(command *cobra.Command) error {
	manager, err := passwordVaultForCommand(command)
	if err != nil {
		return err
	}
	confirmation, _ := command.Flags().GetString("confirm")
	if confirmation != "RESET ENV "+manager.AccountID {
		return invocationError(errors.New("--confirm must exactly match `RESET ENV <account_id>`"))
	}
	password, err := confirmedVaultPassword(command)
	if err != nil {
		return err
	}
	defer clear(password)
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
	_, err = fmt.Fprintln(command.OutOrStdout(), "ENV personal vault reset completed.")
	return err
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
	_, err = fmt.Fprintln(command.OutOrStdout(), "Removed local ENV vault custody; cloud personal and team ciphertext was not changed.")
	return err
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
	password, err := passwordVaultPrompt(command, "New master password")
	if err != nil {
		return nil, err
	}
	confirmation, err := passwordVaultPrompt(command, "Confirm new master password")
	if err != nil {
		clear(password)
		return nil, err
	}
	defer clear(confirmation)
	if len(password) == 0 || !bytes.Equal(password, confirmation) {
		clear(password)
		return nil, errors.New("master passwords must be nonempty and match; vault unchanged")
	}
	return password, nil
}
