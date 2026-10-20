package main

import (
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/inbox"
)

// deliverToThisComputer is `pb send --to <this computer>`: the files are copied into this
// computer's own Inbox folder, where a received file would be listed, with no transfer. A batch is
// delivered whole or not at all.
func deliverToThisComputer(batchID, sourceMachineID, destinationMachineID string, sources []filetransfer.Source) (filetransfer.Batch, error) {
	root, err := configuredInboxPath()
	if err != nil {
		return filetransfer.Batch{}, err
	}
	now := time.Now().UTC()
	batch := filetransfer.Batch{BatchID: batchID}
	rollback := func(cause error) (filetransfer.Batch, error) {
		for _, path := range batch.Paths {
			if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
				cause = fmt.Errorf("%w (and %s could not be removed: %v)", cause, path, removeErr)
			}
		}
		return filetransfer.Batch{}, cause
	}
	for _, source := range sources {
		if _, err := source.Reader.Seek(0, io.SeekStart); err != nil {
			return rollback(err)
		}
		path, err := inbox.Deliver(root, source.Basename, source.Size, source.SHA256, source.Reader)
		if err != nil {
			return rollback(fmt.Errorf("copy %s into the Inbox: %w", source.Basename, err))
		}
		batch.Paths = append(batch.Paths, path)
		batch.Transfers = append(batch.Transfers, filetransfer.Manifest{
			BatchID: batchID, SourceMachineID: sourceMachineID, DestinationMachineID: destinationMachineID,
			Basename: source.Basename, Size: source.Size, SHA256: hex.EncodeToString(source.SHA256[:]),
			CommittedOffset: source.Size, State: "delivered", ReceiptPath: path, CreatedAt: now,
		})
	}
	return batch, nil
}
