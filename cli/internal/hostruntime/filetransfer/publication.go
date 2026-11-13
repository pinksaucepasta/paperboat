package filetransfer

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
	"github.com/pinksaucepasta/paperboat/internal/transfername"
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
	path, err := s.publishToRoot(ctx, transfer)
	if err != nil && ctx.Err() == nil {
		// Nothing was delivered, so the record must not say published: it says failed with the reason, and the spool
		// stays for a repeated complete. A failure to record it must not hide the primary error.
		code := StorageUnavailable
		var failure *Error
		if errors.As(err, &failure) {
			code = failure.Code
		}
		_ = s.config.Store.FailFileTransferPublication(ctx, id, s.config.LocalMachineID, string(code))
	}
	return path, err
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
	if transfer.ReceiptPath == "" || !s.publishedMarked(id) {
		return "", &Error{Code: InvalidPath}
	}
	info, err := os.Lstat(transfer.ReceiptPath)
	if errors.Is(err, os.ErrNotExist) || err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return "", &Error{Code: InvalidPath}
	}
	if err != nil {
		return "", &Error{Code: StorageUnavailable, Cause: err}
	}
	return transfer.ReceiptPath, nil
}

// The published file is a copy, not a second name for the spool content: a hard link would leave a second link inside
// the user-state tree (which the privileged install repair refuses, so a received file blocked every later update) and
// would let an edit of the Inbox file change what the spool serves. A published marker in the spool says the copy at the
// pinned receipt path is complete and ours, which the old same-file test said.
func (s *Service) markerPath(id string) string { return filepath.Join(s.config.Root, id+".published") }

func (s *Service) publishedMarked(id string) bool {
	info, err := os.Lstat(s.markerPath(id))
	return err == nil && info.Mode().IsRegular()
}

// copyToInbox moves the spooled content into the Inbox and links it into place without replacing anything (an
// existing file is ErrExist), so the Inbox never shows a half-written file and a file someone else put there is never
// overwritten. Like taildrop (feature/taildrop/fileops_fs.go Rename) the finished file is renamed, not kept: on the
// same volume the spool content itself becomes the Inbox file; across volumes it is copied and then removed. The
// spool keeps the content on every failure, so a repeated complete publishes it again. A crash after the rename
// leaves only the hidden temporary file, which the next attempt publishes.
func (s *Service) copyToInbox(content, root, path string, id string) error {
	temporary := filepath.Join(root, "."+id+".publish")
	moved := false
	if _, err := os.Lstat(content); err == nil {
		if err := os.Remove(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		//paperboat:allow-source-policy atomic-replacement owner=file-transfer reason=inbox-publish-move
		if moveFile(content, temporary) == nil {
			moved = true
		} else if err := copyFile(content, temporary); err != nil {
			return errors.Join(err, os.Remove(temporary))
		}
	} else if _, tempErr := os.Lstat(temporary); tempErr != nil {
		return err
	} else {
		moved = true
	}
	linkErr := linkFile(temporary, path)
	if linkErr != nil && !errors.Is(linkErr, os.ErrExist) {
		// exFAT and FAT have no hard links (macOS: ENOTSUP, Linux: EPERM). The finished copy is renamed into place
		// instead of copied again after the failure; like taildrop, the check and the rename share one lock.
		if renameErr := renameNoReplace(temporary, path); renameErr == nil {
			return removeIfExists(content) // the copy path left the spool content behind; a moved one has none
		} else {
			linkErr = errors.Join(linkErr, renameErr)
		}
	}
	if linkErr != nil {
		if moved { // keep the content for a repeated complete
			//paperboat:allow-source-policy atomic-replacement owner=file-transfer reason=inbox-publish-move-rollback
			return errors.Join(linkErr, os.Rename(temporary, content))
		}
		return errors.Join(linkErr, os.Remove(temporary))
	}
	return errors.Join(os.Remove(temporary), removeIfExists(content))
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func copyFile(content, temporary string) error {
	source, err := os.Open(content)
	if err != nil {
		return err
	}
	defer source.Close()
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, source)
	if copyErr == nil {
		copyErr = file.Sync()
	}
	return errors.Join(copyErr, file.Close())
}

var renameMu sync.Mutex

// linkFile is replaced by tests to stand in for a file system without hard links.
var linkFile = os.Link

// moveFile is replaced by tests to stand in for an Inbox on another volume (EXDEV).
var moveFile = os.Rename

// renameNoReplace never replaces a file that appeared meanwhile; ErrExist then means what it means for os.Link.
func renameNoReplace(from, to string) error {
	renameMu.Lock()
	defer renameMu.Unlock()
	if _, err := os.Lstat(to); err == nil {
		return fs.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	//paperboat:allow-source-policy atomic-replacement owner=file-transfer reason=inbox-publish-no-hard-links
	return os.Rename(from, to)
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
	// Also for a pinned receipt: its directory is checked again, not trusted because a path was stored. The spool
	// root itself was made private by New.
	if root != s.config.Root {
		if err := s.config.EnsurePublishRoot(root); err != nil {
			return "", &Error{Code: StorageUnavailable, Cause: err}
		}
	}
	content := s.contentPath(transfer.ID)
	// Try the reserved destination first after a restart, before considering a
	// collision. Never consult a new Inbox setting for an existing receipt.
	for index := 0; index <= 10000; index++ {
		path := transfer.ReceiptPath
		if index == 0 && path == "" {
			continue
		}
		if index > 0 {
			path = filepath.Join(root, transfername.Numbered(base, index))
		}
		info, statErr := os.Lstat(path)
		if statErr == nil {
			// Ours only when it is the pinned receipt and its copy was completed; anything else is someone's file.
			if path == transfer.ReceiptPath && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && s.publishedMarked(transfer.ID) {
				return path, nil
			}
			continue
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return "", &Error{Code: StorageUnavailable, Cause: statErr}
		}
		if transfer.ReceiptPath != path {
			if err := s.config.Store.SetFileTransferPublicationPath(ctx, transfer.ID, s.config.LocalMachineID, transfer.ReceiptPath, path); err != nil {
				return "", &Error{Code: StorageUnavailable, Cause: err}
			}
			transfer.ReceiptPath = path
		}
		// The marker comes first: a crash after the copy linked the file must find it marked and keep its name, not
		// publish a second copy as 'name (2)'. Marked but not yet copied is the same state as pinned but not copied.
		marker, err := os.OpenFile(s.markerPath(transfer.ID), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return "", &Error{Code: StorageUnavailable, Cause: err}
		}
		if err := errors.Join(marker.Sync(), marker.Close(), syncDir(s.config.Root)); err != nil {
			return "", &Error{Code: StorageUnavailable, Cause: err}
		}
		if err := s.copyToInbox(content, root, path, transfer.ID); err != nil {
			if errors.Is(err, os.ErrExist) {
				_ = os.Remove(s.markerPath(transfer.ID)) // the marker says the pinned name is ours; someone's file is there
				continue
			}
			if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
				_ = os.Remove(s.markerPath(transfer.ID)) // nothing was linked: the marker must not claim the pinned name
			}
			return "", storageFailure(err) // a full disk is its own permanent code
		}
		if err := syncDir(root); err != nil {
			return "", &Error{Code: StorageUnavailable, Cause: err}
		}
		return path, nil
	}
	return "", &Error{Code: StorageUnavailable, Cause: errors.New("inbox name collision limit reached")}
}
