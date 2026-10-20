package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/inbox"
)

// deliverToThisComputer is `pb send --to <this computer>`: the files are copied into this
// computer's own Inbox folder, where a received file would be listed, with no transfer. A batch is
// delivered whole or not at all.
func deliverToThisComputer(batchID, sourceMachineID, destinationMachineID, userID string, sources []filetransfer.Source) (filetransfer.Batch, error) {
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
			TransferID: selfTransferID(filepath.Base(path)), BatchID: batchID, SourceMachineID: sourceMachineID, DestinationMachineID: destinationMachineID, InitiatingUserID: userID,
			Basename: source.Basename, Size: source.Size, SHA256: hex.EncodeToString(source.SHA256[:]),
			CommittedOffset: source.Size, State: "delivered", ReceiptPath: path, CreatedAt: now,
		})
	}
	return batch, nil
}

// selfTransferID names a file delivered to this computer by the file's place in its Inbox, so the id
// in a send receipt stays valid for `pb send list|status` afterwards.
func selfTransferID(name string) string { return "self_" + inbox.FileID(name) }

// thisComputerTransfers answers `pb send list|status|cancel --on <this computer>`: files sent to the
// computer you are on are copied into its Inbox, so its Inbox is the history.
type thisComputerTransfers struct {
	root, machineID, userID string
}

type transferHistory interface {
	Close() error
	ListPage(ctx context.Context, sessionID string, limit, offset int, query, state string) (filetransfer.HistoryPage, error)
	Status(ctx context.Context, id string) (filetransfer.Manifest, error)
	Cancel(ctx context.Context, id string) error
}

func (t thisComputerTransfers) manifests() ([]filetransfer.Manifest, error) {
	files, err := inbox.Files(t.root)
	if err != nil {
		return nil, err
	}
	manifests := make([]filetransfer.Manifest, 0, len(files))
	for _, file := range files {
		manifests = append(manifests, filetransfer.Manifest{
			TransferID: selfTransferID(file.Name), SourceMachineID: t.machineID, DestinationMachineID: t.machineID, InitiatingUserID: t.userID,
			Basename: file.Name, Size: file.Size, CommittedOffset: file.Size, State: "delivered",
			ReceiptPath: filepath.Join(t.root, file.Name), CreatedAt: file.ReceivedAt,
		})
	}
	return manifests, nil
}

func (t thisComputerTransfers) ListPage(_ context.Context, _ string, limit, offset int, query, state string) (filetransfer.HistoryPage, error) {
	all, err := t.manifests()
	if err != nil {
		return filetransfer.HistoryPage{}, err
	}
	matched := all[:0:0]
	for _, item := range all {
		if (state == "" || state == item.State) && (query == "" || strings.Contains(item.Basename, query) || strings.Contains(item.TransferID, query)) {
			matched = append(matched, item)
		}
	}
	page := filetransfer.HistoryPage{Items: []filetransfer.Manifest{}}
	page.Pagination.Limit, page.Pagination.Offset, page.Pagination.Total = limit, offset, len(matched)
	if offset < len(matched) {
		page.Items = matched[offset:min(offset+limit, len(matched))]
	}
	if next := offset + len(page.Items); len(page.Items) > 0 && next < len(matched) {
		page.Pagination.NextOffset = &next
	}
	return page, nil
}

func (t thisComputerTransfers) Status(_ context.Context, id string) (filetransfer.Manifest, error) {
	all, err := t.manifests()
	if err != nil {
		return filetransfer.Manifest{}, err
	}
	for _, item := range all {
		if item.TransferID == id {
			return item, nil
		}
	}
	return filetransfer.Manifest{}, errors.New("no file with that transfer ID is in this computer's Inbox (it may have been removed)")
}

func (thisComputerTransfers) Close() error { return nil }

func (thisComputerTransfers) Cancel(context.Context, string) error {
	return errors.New("a file sent to this computer is delivered at once; there is nothing to cancel (remove it with `pb inbox`)")
}

// noopCloser stands in for the transfer lease when there is no transfer connection to release.
type noopCloser struct{}

func (noopCloser) Close() error { return nil }
