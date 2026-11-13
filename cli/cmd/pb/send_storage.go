package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/shared/backoff"
)

// destinationStorageFullError is a send the receiving computer refused because its disk has no
// room. Nothing was delivered; the sender stops at once instead of retrying.
type destinationStorageFullError struct {
	machine      string
	needed, free uint64
	// osError is the receiver's operating system's words when it could not store the file for another reason
	// (a read-only file system, access denied); then the disk is not necessarily full.
	osError string
	cause   error
}

func (e destinationStorageFullError) Error() string {
	if e.osError != "" {
		return fmt.Sprintf("%s could not store the file in its Inbox: %s. Nothing was delivered. Fix that disk or point its Inbox at another one with `pb inbox set`, then send again.", e.machine, e.osError)
	}
	if e.needed > 0 {
		return fmt.Sprintf("%s has no room for this in its Inbox (needs %s, %s free). Free space on it or point its Inbox at a bigger disk with `pb inbox set`, then send again. Nothing was delivered.", e.machine, transferSize(int64(e.needed)), transferSize(int64(e.free)))
	}
	return fmt.Sprintf("%s ran out of room in its Inbox while receiving, so nothing was delivered. Free space on it or point its Inbox at a bigger disk with `pb inbox set`, then send again.", e.machine)
}

func (e destinationStorageFullError) Unwrap() error { return e.cause }

// destinationStorageFull turns the receiver's insufficient_storage refusal, or a storage failure it reports
// with the operating system's error, into the typed error that names the computer; any other error is
// returned unchanged.
func destinationStorageFull(machine string, err error) error {
	var remote *filetransfer.Error
	if err == nil || !errors.As(err, &remote) {
		return err
	}
	if remote.Code == "storage_unavailable" && remote.OSError != "" {
		return destinationStorageFullError{machine: machine, osError: remote.OSError, cause: err}
	}
	if remote.Code != "insufficient_storage" {
		return err
	}
	return destinationStorageFullError{machine: machine, needed: remote.NeededBytes, free: remote.FreeBytes, cause: err}
}

// destinationGoneError is a running send whose destination the server reports removed or revoked: unlike an offline
// machine (the send waits for it), it never comes back, so the send stops at once.
type destinationGoneError struct {
	machine string
	cause   error
}

func (e destinationGoneError) Error() string {
	return fmt.Sprintf("%s was removed or revoked, so the transfer stopped; nothing more is sent to it.", e.machine)
}

func (e destinationGoneError) Unwrap() error { return e.cause }

// destinationGone turns the server's user_machine_not_found during a send into destinationGoneError; any other error
// is returned unchanged.
func destinationGone(machine string, err error) error {
	var remote *api.APIError
	if err == nil || !errors.As(err, &remote) || remote.Code != "user_machine_not_found" {
		return err
	}
	return destinationGoneError{machine: machine, cause: err}
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
	if !isDestinationBusy(err) {
		return err
	}
	return destinationBusyError{machine: machine, cause: err}
}

func isDestinationBusy(err error) bool {
	var remote *filetransfer.Error
	return err != nil && errors.As(err, &remote) && remote.Code == "resource_limit"
}

// A destination takes 2 transfers at once and refuses a third at once; a send queues behind them instead (F21-166: 6
// parallel sends to one machine all finish): it asks again every few seconds, says so once, and gives up with the
// busy refusal after sendQueueWait. Nothing is delivered by a refused attempt, so asking again is safe.
var (
	sendQueueWait = 15 * time.Minute
	sendQueuePoll = 3 * time.Second
)

// queueWhileBusy runs attempt until the destination has a free slot. Any other result, and the last busy refusal after
// the wait, is returned as it came.
func queueWhileBusy(ctx context.Context, machine string, waiting func(string), attempt func() error) error {
	deadline := time.Now().Add(sendQueueWait)
	for {
		err := attempt()
		if !isDestinationBusy(err) || !time.Now().Before(deadline) {
			return err
		}
		waiting(fmt.Sprintf("%s is already receiving as many transfers as it accepts at once: waiting for one to finish (up to %s)", machine, sendQueueWait.Round(time.Minute)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff.Jitter(sendQueuePoll)):
		}
	}
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

// destinationFileTooLargeError is a send the receiving computer refused because one file is bigger than its
// Inbox's file system can hold (FAT32: 4 GiB minus one byte). Nothing was sent.
type destinationFileTooLargeError struct {
	machine, fileSystem string
	size, limit         uint64
	cause               error
}

func (e destinationFileTooLargeError) Error() string {
	return fmt.Sprintf("%s keeps its Inbox on a %s volume, which cannot hold a file over %s (this one is %s). Send a smaller file, or point its Inbox at a disk with NTFS, exFAT, APFS or ext4 with `pb inbox set`. Nothing was sent.", e.machine, e.fileSystem, transferSize(int64(e.limit)), transferSize(int64(e.size)))
}

func (e destinationFileTooLargeError) Unwrap() error { return e.cause }

// destinationFileTooLarge turns the receiver's file_too_large refusal into the typed error that names the
// computer; any other error is returned unchanged.
func destinationFileTooLarge(machine string, err error) error {
	var remote *filetransfer.Error
	if err == nil || !errors.As(err, &remote) || remote.Code != "file_too_large" {
		return err
	}
	return destinationFileTooLargeError{machine: machine, fileSystem: remote.FileSystem, size: remote.FileBytes, limit: remote.FileLimit, cause: err}
}
