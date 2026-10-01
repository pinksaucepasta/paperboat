package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/prompt"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const confirmationLifetime = 5 * time.Minute
const confirmationAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

type confirmationCommandKey struct{}

type confirmationChallenge struct {
	Token     string    `json:"token"`
	Command   string    `json:"command"`
	Server    string    `json:"server"`
	Scope     string    `json:"scope"`
	ExpiresAt time.Time `json:"expires_at"`
}

func confirmationPath(command *cobra.Command) (string, string, error) {
	configPath, _ := command.Flags().GetString("config")
	cfg, err := config.Load(configPath)
	if err != nil {
		return "", "", err
	}
	server, _ := command.Flags().GetString("server")
	if server == "" {
		server = cfg.ServerURL
	}
	if server != "" {
		server, err = config.NormalizeServerURL(server)
		if err != nil {
			return "", "", err
		}
	}
	return cfg.Path() + ".confirmation.json", server, nil
}

func randomConfirmationCode() (string, error) {
	var values [6]byte
	if _, err := rand.Read(values[:]); err != nil {
		return "", err
	}
	var token [6]byte
	for i, value := range values {
		token[i] = confirmationAlphabet[int(value)%len(confirmationAlphabet)]
	}
	return string(token[:]), nil
}

func validConfirmationCode(token string) bool {
	if len(token) != 6 {
		return false
	}
	for _, char := range token {
		if !strings.ContainsRune(confirmationAlphabet, char) {
			return false
		}
	}
	return true
}

func confirmationCommandLine(command *cobra.Command, token string, explicitArgs []string, identity bool) string {
	parts := []string{"pb"}
	for _, name := range []string{"config", "server", "no-customization"} {
		if flag := command.Flags().Lookup(name); flag != nil && flag.Changed {
			if flag.Value.Type() == "bool" {
				parts = append(parts, "--"+name)
			} else {
				parts = append(parts, "--"+name, shellQuote(flag.Value.String()))
			}
		}
	}
	parts = append(parts, strings.TrimPrefix(command.CommandPath(), "pb "))
	args := command.Flags().Args()
	if explicitArgs != nil {
		args = explicitArgs
	}
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	flags := make([]string, 0)
	command.Flags().Visit(func(flag *pflag.Flag) {
		switch flag.Name {
		case "config", "server", "no-customization", "confirm":
			return
		}
		if identity && (flag.Name == "json" || flag.Name == "wait" || flag.Name == "timeout") {
			return
		}
		if flag.Value.Type() == "bool" {
			if flag.Value.String() == "true" {
				flags = append(flags, "--"+flag.Name)
			} else {
				flags = append(flags, "--"+flag.Name+"=false")
			}
		} else {
			flags = append(flags, "--"+flag.Name+"="+shellQuote(flag.Value.String()))
		}
	})
	sort.Strings(flags)
	parts = append(parts, flags...)
	if token != "" {
		parts = append(parts, "--confirm", token)
	}
	return strings.Join(parts, " ")
}

func confirmMutation(command *cobra.Command, scope, impact string) error {
	return confirmMutationWithArgs(command, scope, impact, nil)
}

func confirmMutationWithArgs(command *cobra.Command, scope, impact string, explicitArgs []string) error {
	if interactive, _ := command.Context().Value(configSyncInteractiveConfirmation{}).(bool); interactive {
		confirmed, err := prompt.Confirm(prompt.ConfirmOptions{Context: command.Context(), Title: "Confirm configuration change", Description: impact, Output: command.ErrOrStderr()})
		if err != nil {
			return err
		}
		if !confirmed {
			return prompt.ErrCanceled
		}
		return nil
	}
	basePath, server, err := confirmationPath(command)
	if err != nil {
		return err
	}
	token, _ := command.Flags().GetString("confirm")
	current := confirmationCommandLine(command, "", explicitArgs, true)
	if token == "" {
		if err := os.MkdirAll(filepath.Dir(basePath), 0o700); err != nil {
			return err
		}
		removeExpiredConfirmations(basePath)
		var file *os.File
		for attempt := 0; attempt < 4; attempt++ {
			token, err = randomConfirmationCode()
			if err != nil {
				return err
			}
			file, err = os.OpenFile(basePath+"."+token, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if !errors.Is(err, os.ErrExist) {
				break
			}
		}
		if err != nil {
			return err
		}
		challenge := confirmationChallenge{Token: token, Command: current, Server: server, Scope: scope, ExpiresAt: time.Now().Add(confirmationLifetime)}
		if err := json.NewEncoder(file).Encode(challenge); err != nil {
			file.Close()
			os.Remove(file.Name())
			return err
		}
		if err := file.Close(); err != nil {
			os.Remove(file.Name())
			return err
		}
		copyCommand := confirmationCommandLine(command, token, explicitArgs, false)
		if jsonOutputRequested(command) {
			return writeConfirmationRequired(command, token, scope, impact, challenge.ExpiresAt, copyCommand)
		}
		fmt.Fprintf(command.OutOrStdout(), "%s\nNothing has changed. To confirm within 5 minutes, run:\n%s\n", impact, copyCommand)
		return exitCodeError{code: 2}
	}
	if !validConfirmationCode(token) {
		return invocationError(errors.New("--confirm requires the six-character code shown by the preview"))
	}
	path := basePath + "." + token
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("confirmation code missing or already used; run the command without --confirm again")
		}
		return err
	}
	var pending confirmationChallenge
	if json.Unmarshal(data, &pending) != nil || pending.Token != token {
		return errors.New("invalid confirmation code; run the command without --confirm again")
	}
	claimed := fmt.Sprintf("%s.used.%d", path, os.Getpid())
	if err := os.Rename(path, claimed); err != nil {
		return err
	}
	defer os.Remove(claimed)
	data, err = os.ReadFile(claimed)
	if err != nil {
		return err
	}
	var challenge confirmationChallenge
	if json.Unmarshal(data, &challenge) != nil {
		return errors.New("invalid confirmation state; run the command without --confirm again")
	}
	if time.Now().After(challenge.ExpiresAt) {
		return errors.New("confirmation code expired; run the command without --confirm again")
	}
	if challenge.Token != token || challenge.Command != current || challenge.Server != server || challenge.Scope != scope {
		return errors.New("confirmation code does not match the current command, target, or resource state; run the command without --confirm again")
	}
	return nil
}

func removeExpiredConfirmations(basePath string) {
	paths, err := filepath.Glob(basePath + ".??????")
	if err != nil {
		return
	}
	for _, path := range paths {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			continue
		}
		var challenge confirmationChallenge
		if json.Unmarshal(data, &challenge) == nil && validConfirmationCode(challenge.Token) && path == basePath+"."+challenge.Token && time.Now().After(challenge.ExpiresAt) {
			_ = os.Remove(path)
		}
	}
}

func confirmContextMutation(c *command.Context, scope, impact string) error {
	return confirmContextMutationWithArgs(c, scope, impact, nil)
}

func confirmContextMutationWithArgs(c *command.Context, scope, impact string, explicitArgs []string) error {
	command, ok := c.Context.Value(confirmationCommandKey{}).(*cobra.Command)
	if !ok {
		return errors.New("confirmation requires a CLI command context")
	}
	return confirmMutationWithArgs(command, scope, impact, explicitArgs)
}

func writeConfirmationRequired(command *cobra.Command, token, scope, impact string, expires time.Time, copyCommand string) error {
	result := cliJSONEnvelope{SchemaVersion: cliJSONSchemaVersion, OK: false,
		Data:  map[string]any{"outcome": "confirmation_required", "confirmation_token": token, "scope": scope, "impact": impact, "expires_at": expires, "confirmation_command": copyCommand},
		Error: &cliJSONError{Code: "confirmation_required", Category: "user_action", Message: "Confirmation is required before this operation changes resources.", Retryable: true, StateChanged: false, Recovery: copyCommand},
	}
	if err := json.NewEncoder(command.OutOrStdout()).Encode(result); err != nil {
		return err
	}
	return exitCodeError{code: 2}
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

func sortedSessionScope(ids []string) string {
	ids = append([]string(nil), ids...)
	sort.Strings(ids)
	return strings.Join(ids, ",")
}
