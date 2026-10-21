package main

import (
	"errors"
	"fmt"

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
