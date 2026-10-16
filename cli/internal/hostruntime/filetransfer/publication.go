package filetransfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
)

// Inbox changes apply when a transfer first publishes. Its exact destination is
// then durable: settings changes and retries must never move or duplicate it.
func (s *Service) PublishedPath(ctx context.Context, id string) (string, error) {
	release, err := s.admitMutation(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	transfer, err := s.config.Store.FileTransfer(ctx, id)
	if err != nil {
		return "", lookupFailure(err)
	}
	lock := s.lock("batch:" + transfer.BatchID)
	lock.Lock()
	defer lock.Unlock()
	transfer, err = s.config.Store.FileTransfer(ctx, id)
	if err != nil {
		return "", lookupFailure(err)
	}
	if transfer.DestinationMachineID != s.config.LocalMachineID || transfer.State != "published" {
		return "", &Error{Code: InvalidPath}
	}
	return s.publishToRoot(ctx, transfer)
}

// ExistingPublishedPath is read-only, including when the user removed the file.
func (s *Service) ExistingPublishedPath(ctx context.Context, id string) (string, error) {
	transfer, err := s.config.Store.FileTransfer(ctx, id)
	if err != nil {
		return "", lookupFailure(err)
	}
	if transfer.DestinationMachineID != s.config.LocalMachineID || transfer.State != "published" {
		return "", &Error{Code: InvalidPath}
	}
	contentInfo, err := os.Stat(s.contentPath(id))
	if err != nil {
		return "", &Error{Code: StorageUnavailable, Cause: err}
	}
	if transfer.ReceiptPath != "" {
		matches, err := publishedFileMatches(contentInfo, transfer.ReceiptPath)
		if err != nil {
			return "", &Error{Code: StorageUnavailable, Cause: err}
		}
		if !matches {
			return "", &Error{Code: InvalidPath}
		}
		return transfer.ReceiptPath, nil
	}
	root, base, err := s.publicationRoot(transfer)
	if err != nil {
		return "", err
	}
	for index := 1; index <= 10000; index++ {
		path := filepath.Join(root, publishedName(base, index))
		matches, err := publishedFileMatches(contentInfo, path)
		if err != nil {
			return "", &Error{Code: StorageUnavailable, Cause: err}
		}
		if matches {
			return path, nil
		}
	}
	return "", &Error{Code: InvalidPath}
}

func publishedFileMatches(contentInfo os.FileInfo, path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && os.SameFile(contentInfo, info), nil
}

func (s *Service) publicationRoot(transfer store.FileTransfer) (string, string, error) {
	root, base := s.config.Root, transfer.ID+"-"+transfer.Basename
	if s.config.PublishRoot != nil {
		var err error
		root, err = s.config.PublishRoot()
		if err != nil {
			return "", "", &Error{Code: StorageUnavailable, Cause: err}
		}
		base = transfer.Basename
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", "", &Error{Code: StorageUnavailable, Cause: errors.New("publish root is invalid")}
	}
	return root, base, nil
}

func (s *Service) publishToRoot(ctx context.Context, transfer store.FileTransfer) (string, error) {
	var root, base string
	if transfer.ReceiptPath != "" {
		if !filepath.IsAbs(transfer.ReceiptPath) || filepath.Clean(transfer.ReceiptPath) != transfer.ReceiptPath {
			return "", &Error{Code: StorageUnavailable, Cause: errors.New("published receipt path is invalid")}
		}
		root, base = filepath.Dir(transfer.ReceiptPath), transfer.Basename
	} else {
		var err error
		root, base, err = s.publicationRoot(transfer)
		if err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", &Error{Code: StorageUnavailable, Cause: err}
	}
	if err := os.Chmod(root, 0700); err != nil {
		return "", &Error{Code: StorageUnavailable, Cause: err}
	}
	content := s.contentPath(transfer.ID)
	contentInfo, err := os.Stat(content)
	if err != nil {
		return "", &Error{Code: StorageUnavailable, Cause: err}
	}
	// Try the reserved destination first after a restart, before considering a
	// collision. Never consult a new Inbox setting for an existing receipt.
	for index := 0; index <= 10000; index++ {
		path := transfer.ReceiptPath
		if index == 0 && path == "" {
			continue
		}
		if index > 0 {
			path = filepath.Join(root, publishedName(base, index))
		}
		info, statErr := os.Lstat(path)
		if statErr == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(contentInfo, info)) {
			continue
		}
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return "", &Error{Code: StorageUnavailable, Cause: statErr}
		}
		if transfer.ReceiptPath != path {
			if err := s.config.Store.SetFileTransferPublicationPath(ctx, transfer.ID, s.config.LocalMachineID, transfer.ReceiptPath, path); err != nil {
				return "", &Error{Code: StorageUnavailable, Cause: err}
			}
			transfer.ReceiptPath = path
		}
		if statErr == nil {
			return path, nil
		}
		if err := os.Link(content, path); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", &Error{Code: StorageUnavailable, Cause: err}
		}
		if err := syncDir(root); err != nil {
			return "", &Error{Code: StorageUnavailable, Cause: err}
		}
		return path, nil
	}
	return "", &Error{Code: StorageUnavailable, Cause: errors.New("inbox name collision limit reached")}
}
