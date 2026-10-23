package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/filetransfer"
)

// destinationStorageFullError is a send the receiving computer refused because its disk has no
// room. Nothing was delivered; the sender stops at once instead of retrying.
type destinationStorageFullError struct {
	machine      string
	needed, free uint64
	cause        error
}

func (e destinationStorageFullError) Error() string {
	if e.needed > 0 {
		return fmt.Sprintf("%s has no room for this in its Inbox (needs %s, %s free). Free space on it or point its Inbox at a bigger disk with `pb inbox set`, then send again. Nothing was delivered.", e.machine, transferSize(int64(e.needed)), transferSize(int64(e.free)))
	}
	return fmt.Sprintf("%s ran out of room in its Inbox while receiving, so nothing was delivered. Free space on it or point its Inbox at a bigger disk with `pb inbox set`, then send again.", e.machine)
}

func (e destinationStorageFullError) Unwrap() error { return e.cause }

// destinationStorageFull turns the receiver's insufficient_storage refusal into the typed error that
// names the computer; any other error is returned unchanged.
func destinationStorageFull(machine string, err error) error {
	var remote *filetransfer.Error
	if err == nil || !errors.As(err, &remote) || remote.Code != "insufficient_storage" {
		return err
	}
	return destinationStorageFullError{machine: machine, needed: remote.NeededBytes, free: remote.FreeBytes, cause: err}
}

// destinationBusyError is a send the receiving computer refused because it already holds as many
// unfinished transfers as it accepts (its resource_limit, HTTP 429). Nothing new was delivered.
type destinationBusyError struct {
	machine string
	cause   error
}

func (e destinationBusyError) Error() string {
	return fmt.Sprintf("%s is already receiving as many transfers as it accepts at once. Let them finish, or cancel unfinished ones (`pb send list --on %s`, then `pb send cancel <transfer-id> --on %s`), then send again.", e.machine, e.machine, e.machine)
}

func (e destinationBusyError) Unwrap() error { return e.cause }

// destinationBusy turns the receiver's resource_limit refusal into destinationBusyError; any other
// error is returned unchanged.
func destinationBusy(machine string, err error) error {
	var remote *filetransfer.Error
	if err == nil || !errors.As(err, &remote) || remote.Code != "resource_limit" {
		return err
	}
	return destinationBusyError{machine: machine, cause: err}
}

// serverTransferLimit turns the server's 413 file_transfer_limit_exceeded (a team policy can be
// stricter than the CLI's defaults) into the same typed refusal a local check produces, naming the
// limit that was actually exceeded. It returns nil for any other error or when no limit is hit.
func serverTransferLimit(err error, sources []filetransfer.Source) error {
	var refusal *api.APIError
	if len(sources) == 0 || !errors.As(err, &refusal) || refusal.Code != "file_transfer_limit_exceeded" {
		return nil
	}
	number := func(key string) int64 {
		switch value := refusal.Details[key].(type) {
		case float64:
			return int64(value)
		case json.Number:
			parsed, _ := value.Int64()
			return parsed
		}
		return 0
	}
	limits := filetransfer.Limits{MaxFileBytes: number("max_file_bytes"), MaxBatchFiles: int(number("max_batch_files")), MaxBatchBytes: number("max_batch_bytes")}
	var limit *filetransfer.LimitError
	if errors.As(filetransfer.ValidateSources(sources, limits), &limit) {
		return limit
	}
	return nil
}
