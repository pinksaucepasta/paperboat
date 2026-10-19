package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/inbox"
)

var errOwnerOnlyFileInvalid = errors.New("invalid owner-only file")

func readOwnerOnlyFile(path string, limit int64) (data []byte, resultErr error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := validateOwnerOnlyRegularFile(path, info); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			clear(data)
			data = nil
			resultErr = errors.Join(resultErr, closeErr)
		}
	}()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, errOwnerOnlyFileInvalid
	}
	if err := validateOwnerOnlyRegularFile(path, opened); err != nil {
		return nil, err
	}
	data, err = io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		clear(data)
		return nil, err
	}
	if int64(len(data)) > limit {
		clear(data)
		return nil, errOwnerOnlyFileInvalid
	}
	return data, nil
}

func inboxRequestsCommand(c *command.Context) error {
	if c.Args().Len() != 0 {
		return localArgumentError("pb inbox requests does not accept arguments")
	}
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	requests, err := client.TeamInboxRequests(c.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": map[string]any{"requests": requests}})
	}
	if len(requests) == 0 {
		fmt.Fprintln(c.Writer, "No team file requests.")
		return nil
	}
	w := tabwriter.NewWriter(c.Writer, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "REQUEST\tSENDER\tFILES\tSTATUS\tEXPIRES")
	for _, request := range requests {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", request.RequestID, request.SenderAccount, len(request.Files), request.Status, relativeTimestamp(request.ExpiresAt))
	}
	return w.Flush()
}

func inboxDecisionCommand(c *command.Context, action string) error {
	if c.Args().Len() != 1 {
		return localArgumentError("usage: pb inbox approve|decline <request-id> --generation <generation>")
	}
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	generation := uint64(c.Uint("generation"))
	if generation == 0 {
		request, getErr := client.TeamInboxRequest(c.Context, c.Args().First())
		if getErr != nil {
			return friendlyCommandError(getErr)
		}
		generation = request.DecisionGeneration
	}
	request, err := client.DecideTeamInboxRequest(c.Context, c.Args().First(), action, generation)
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": request})
	}
	fmt.Fprintf(c.Writer, "%s: %s\n", request.RequestID, request.Status)
	return nil
}

func inboxPolicyCommand(c *command.Context) error {
	if c.Args().Len() > 1 {
		return localArgumentError("usage: pb inbox policy [manual|automatic] [--receipt-email]")
	}
	if c.Args().Len() == 1 && c.Args().First() != "manual" && c.Args().First() != "automatic" {
		return localArgumentError("acceptance must be manual or automatic")
	}
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	policy, err := client.TeamInboxPolicy(c.Context)
	if err != nil {
		return friendlyCommandError(err)
	}
	if c.Args().Len() == 1 {
		acceptance := c.Args().First()
		policy.Acceptance, policy.ReceiptEmail = acceptance, c.Bool("receipt-email")
		policy, err = client.SetTeamInboxPolicy(c.Context, policy)
		if err != nil {
			return friendlyCommandError(err)
		}
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": policy})
	}
	fmt.Fprintf(c.Writer, "Acceptance: %s\nReceipt email: %t\n", policy.Acceptance, policy.ReceiptEmail)
	return nil
}

func configuredInboxPath() (string, error) {
	store, err := runtimeIdentityStore()
	if err != nil {
		return "", err
	}
	registration, err := store.Registration()
	if err != nil {
		return "", commandPreparationFailure{step: prepareInboxRegistration, cause: err}
	}
	if err := inbox.ValidatePath(registration.InboxPath); err != nil {
		return "", err
	}
	return registration.InboxPath, nil
}

func inboxPathCommand(c *command.Context) error {
	path, err := configuredInboxPath()
	if err != nil {
		return err
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": map[string]string{"path": path}})
	}
	fmt.Fprintln(c.Writer, path)
	return nil
}

func inboxSetCommand(c *command.Context) error {
	if c.Args().Len() != 1 {
		return localArgumentError("usage: pb inbox set <directory>")
	}
	return setInboxPath(c, c.Args().First())
}

func inboxResetCommand(c *command.Context) error {
	if c.Args().Len() != 0 {
		return localArgumentError("pb inbox reset does not accept arguments")
	}
	path, err := inbox.DefaultPath()
	if err != nil {
		return err
	}
	return setInboxPath(c, path)
}

func setInboxPath(c *command.Context, path string) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if err := inbox.EnsurePath(path); err != nil {
		return err
	}
	store, err := runtimeIdentityStore()
	if err != nil {
		return err
	}
	registration, err := store.Registration()
	if err != nil {
		return commandPreparationFailure{step: prepareInboxRegistration, cause: err}
	}
	registration.InboxPath = path
	registration.UpdatedAt = time.Now().UTC()
	if err := store.SaveRegistration(registration); err != nil {
		return err
	}
	if c.Bool("json") {
		return json.NewEncoder(c.Writer).Encode(map[string]any{"schema_version": "1.0", "ok": true, "data": map[string]string{"path": path}})
	}
	fmt.Fprintln(c.Writer, path)
	return nil
}
