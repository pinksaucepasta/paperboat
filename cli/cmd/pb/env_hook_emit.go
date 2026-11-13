package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/runtime"
	"github.com/spf13/cobra"
)

// envHookBudget bounds the whole hook so shell startup never waits longer.
// A cached answer from the local runtime takes a few milliseconds.
const envHookBudget = 1500 * time.Millisecond

// envHookResult is what one hook run found. reason is a short user-facing
// cause; quiet results (no login, other account) print nothing at all.
type envHookResult struct {
	values []string
	names  []runtime.EnvironmentName // only from the names request
	reason string
	quiet  bool
	// disabled: the workspace turned off "Commands on machines can list ENV names".
	disabled bool
}

func (r envHookResult) summary() string {
	switch {
	case r.quiet:
		return "Nothing loads: the logged-in Paperboat account is not the one recorded, or this machine belongs to another account."
	case r.reason != "":
		return "Nothing loads right now: " + r.reason + "."
	}
	return fmt.Sprintf("%d values load.", len(r.values))
}

// emitEnvironmentHook prints only shell assignments on stdout. Any failure
// leaves stdout empty so the shell's eval is a no-op, and says one short
// line on stderr when a person is watching.
func emitEnvironmentHook(command *cobra.Command, shell string) {
	if shell != "bash" && shell != "zsh" && shell != "fish" && shell != envHookPowerShell {
		return
	}
	ctx, cancel := context.WithTimeout(command.Context(), envHookBudget)
	defer cancel()
	record, err := readEnvHookRecord(command)
	if err != nil {
		return
	}
	result := loadEnvironmentHook(ctx, command, record)
	if result.quiet {
		return
	}
	if result.reason != "" {
		writeEnvHookNotice(fmt.Sprintf("Paperboat ENV not loaded: %s. Run `pb env hook --status`.\n", result.reason))
		return
	}
	var out strings.Builder
	for _, entry := range result.values {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !envHookName(name) {
			continue
		}
		out.WriteString(envHookAssignment(shell, name, value))
	}
	_, _ = io.WriteString(command.OutOrStdout(), out.String())
}

// envHookAssignment is one `set the variable` line for shell; value is always
// a quoted literal, never code.
func envHookAssignment(shell, name, value string) string {
	switch shell {
	case "fish":
		return "set -gx " + name + " " + fishSingleQuote(value) + "\n"
	case envHookPowerShell:
		return "$env:" + name + " = " + powerShellValueLiteral(value) + "\n"
	}
	return "export " + name + "=" + shellSingleQuote(value) + "\n"
}

func readEnvHookRecord(command *cobra.Command) (envHookRecord, error) {
	var record envHookRecord
	path, err := envHookRecordPath(command)
	if err != nil {
		return record, err
	}
	data, err := readOwnerOnlyFile(path, 4096)
	if err != nil {
		return record, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, err
	}
	if record.Schema != envHookSchema || record.AccountID == "" || record.WorkspaceID == "" {
		return record, errors.New("invalid ENV hook record")
	}
	return record, nil
}

// loadEnvironmentHook loads values only while the account logged in to pb for
// this unix user is still the recorded one; the runtime then refuses any
// account but its own.
func loadEnvironmentHook(ctx context.Context, command *cobra.Command, record envHookRecord) envHookResult {
	return loadFromRuntime(ctx, command, record, runtime.ShellEnvironmentPath)
}

// loadEnvironmentNames asks the same runtime for the names only (`pb env names`).
func loadEnvironmentNames(ctx context.Context, command *cobra.Command, record envHookRecord) envHookResult {
	return loadFromRuntime(ctx, command, record, runtime.ShellEnvironmentNamesPath)
}

func loadFromRuntime(ctx context.Context, command *cobra.Command, record envHookRecord, path string) envHookResult {
	cfg, err := workspaceConfig(configPathFlag(command), "")
	if err != nil {
		return envHookResult{quiet: true}
	}
	account, err := workspaceAccountID(cfg)
	if err != nil || account == "" || account != record.AccountID || cfg.ServerURL != record.ServerURL {
		return envHookResult{quiet: true}
	}
	endpoint, token, err := workerLocalEndpoint()
	if err != nil {
		return envHookResult{reason: "the Paperboat runtime is not running"}
	}
	return requestShellEnvironment(ctx, endpoint, token, path, record)
}

func requestShellEnvironment(ctx context.Context, endpoint, token, path string, record envHookRecord) envHookResult {
	body, err := json.Marshal(map[string]string{"workspace_id": record.WorkspaceID, "account_id": record.AccountID})
	if err != nil {
		return envHookResult{reason: "the request could not be built"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+path, bytes.NewReader(body))
	if err != nil {
		return envHookResult{reason: "the request could not be built"}
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return envHookResult{reason: "the Paperboat runtime did not answer in time"}
		}
		return envHookResult{reason: "the Paperboat runtime is not running"}
	}
	defer response.Body.Close()
	var envelope struct {
		Data *struct {
			Values []string                  `json:"values"`
			Names  []runtime.EnvironmentName `json:"names"`
		} `json:"data"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&envelope); err != nil {
		return envHookResult{reason: "the Paperboat runtime sent an unreadable answer"}
	}
	if response.StatusCode == http.StatusOK && envelope.Data != nil {
		return envHookResult{values: envelope.Data.Values, names: envelope.Data.Names}
	}
	code := ""
	if envelope.Error != nil {
		code = envelope.Error.Code
	}
	switch code {
	case "account_mismatch":
		return envHookResult{quiet: true}
	case "names_listing_disabled":
		return envHookResult{disabled: true}
	case "environment_pending":
		return envHookResult{reason: "ENV needs Paperboat online, or run `pb env vault unlock` (delivery to this machine is pending)"}
	case "environment_undeliverable":
		return envHookResult{reason: "ENV delivery to this machine failed; see `pb env host show`"}
	}
	return envHookResult{reason: "the Paperboat runtime refused the request; update pb or run `pb doctor`"}
}

// workerLocalEndpoint reads the loopback address and owner-only control token
// the runtime worker publishes in its state directory.
func workerLocalEndpoint() (string, string, error) {
	stateRoot, err := previewRuntimeStateRoot()
	if err != nil {
		return "", "", err
	}
	data, err := readOwnerOnlyFile(filepath.Join(stateRoot, "runtime", "worker-local.json"), 4096)
	if err != nil {
		return "", "", err
	}
	var local struct {
		Schema        string `json:"schema"`
		ListenAddress string `json:"listen_address"`
	}
	if err := json.Unmarshal(data, &local); err != nil || local.Schema != "paperboat.worker-local/v1" {
		return "", "", errors.New("invalid worker-local descriptor")
	}
	host, _, err := net.SplitHostPort(local.ListenAddress)
	if err != nil || host != "127.0.0.1" && host != "::1" {
		return "", "", errors.New("worker-local address is not loopback")
	}
	token, err := readOwnerOnlyFile(filepath.Join(stateRoot, "runtime", "local-control-token"), 1024)
	if err != nil || strings.TrimSpace(string(token)) == "" {
		return "", "", errors.Join(errors.New("worker control token unavailable"), err)
	}
	return "http://" + local.ListenAddress, strings.TrimSpace(string(token)), nil
}

func envHookName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// powerShellSingleQuote is a literal: PowerShell interpolates nothing inside
// single quotes, and its parser treats the four typographic single quotes as
// quote characters too, so each is doubled like '. Line breaks become
// [char]10 and [char]13 joins, because PowerShell splits native output into
// lines and would rejoin them with CRLF, changing the value.
func powerShellSingleQuote(value string) string {
	return "'" + strings.NewReplacer("'", "''", "\u2018", "\u2018\u2018", "\u2019", "\u2019\u2019",
		"\u201a", "\u201a\u201a", "\u201b", "\u201b\u201b", "\r", "' + [char]13 + '", "\n", "' + [char]10 + '").Replace(value) + "'"
}

// powerShellValueLiteral is a value as ASCII only: PowerShell decodes the native output of the hook line with the console's
// OEM code page (437 over ssh), which turned 'emoji \U0001F680' into mojibake in a plain ssh session on Windows (X lane, X-11).
// Non-ASCII characters therefore travel as code points, line breaks as [char] joins (see powerShellSingleQuote).
func powerShellValueLiteral(value string) string {
	var out strings.Builder
	out.WriteByte('\'')
	for _, r := range value {
		switch {
		case r == '\'':
			out.WriteString("''")
		case r == '\r':
			out.WriteString("' + [char]13 + '")
		case r == '\n':
			out.WriteString("' + [char]10 + '")
		case r < 0x80:
			out.WriteRune(r)
		default:
			fmt.Fprintf(&out, "' + [char]::ConvertFromUtf32(0x%X) + '", r)
		}
	}
	out.WriteByte('\'')
	return out.String()
}

func fishSingleQuote(value string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(value) + "'"
}

// envHookTerminal is where the one-line notice goes. The hook line discards
// pb's stderr so an old or broken pb never adds noise to shell startup; the
// notice goes to the controlling terminal, which non-interactive ssh and VS
// Code's environment probe do not have.
var envHookTerminal = envHookConsole

func writeEnvHookNotice(message string) {
	tty, err := os.OpenFile(envHookTerminal, os.O_WRONLY, 0)
	if err != nil {
		return
	}
	_, _ = io.WriteString(tty, message)
	_ = tty.Close()
}
