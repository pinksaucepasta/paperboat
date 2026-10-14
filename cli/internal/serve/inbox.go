package serve

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxInboxNameAttempts = 1000

var ErrInboxCollision = errors.New("no collision-free serve Inbox name available")

type InboxCopy struct {
	Path   string
	Size   int64
	SHA256 [sha256.Size]byte
}

func PlanInboxCopy(source Source, inboxPath string) (string, error) {
	if source.Kind != SourceFile || !filepath.IsAbs(inboxPath) {
		return "", ErrInvalidSource
	}
	if err := source.Revalidate(); err != nil {
		return "", err
	}
	return availableInboxPath(filepath.Join(filepath.Clean(inboxPath), "serve"), filepath.Base(source.Path))
}

func CopyFileToInbox(ctx context.Context, source Source, inboxPath string) (result InboxCopy, returnErr error) {
	return copyFileToInbox(ctx, source, inboxPath, hashFile)
}

func copyFileToInbox(ctx context.Context, source Source, inboxPath string, verify func(string) (int64, [sha256.Size]byte, error)) (result InboxCopy, returnErr error) {
	if ctx == nil || source.Kind != SourceFile || !filepath.IsAbs(inboxPath) {
		return InboxCopy{}, ErrInvalidSource
	}
	if verify == nil {
		return InboxCopy{}, ErrInvalidSource
	}
	if err := source.Revalidate(); err != nil {
		return InboxCopy{}, err
	}
	directory := filepath.Join(filepath.Clean(inboxPath), "serve")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return InboxCopy{}, serveOperationFailure("could not prepare selected serve Inbox", err)
	}
	input, err := os.Open(source.Path)
	if err != nil {
		return InboxCopy{}, serveOperationFailure("could not open selected serve source", err)
	}
	inputOpen := true
	defer func() {
		if inputOpen {
			if closeErr := input.Close(); closeErr != nil {
				returnErr = errors.Join(returnErr, serveOperationFailure("could not close selected serve source", closeErr))
			}
		}
	}()
	openedInfo, err := input.Stat()
	if err != nil {
		return InboxCopy{}, serveOperationFailure("selected serve source changed or became unavailable", ErrSourceChanged, err)
	}
	if !os.SameFile(source.info, openedInfo) {
		return InboxCopy{}, ErrSourceChanged
	}
	//paperboat:allow-source-policy atomic-replacement owner=serve-inbox reason=streamed-file-publication
	temporary, err := os.CreateTemp(directory, ".copy-*")
	if err != nil {
		return InboxCopy{}, serveOperationFailure("could not create a temporary serve Inbox file", err)
	}
	temporaryPath := temporary.Name()
	temporaryOpen := true
	defer func() {
		if temporaryOpen {
			if closeErr := temporary.Close(); closeErr != nil {
				returnErr = errors.Join(returnErr, serveOperationFailure("could not close temporary serve Inbox file", closeErr))
			}
		}
		if removeErr := os.Remove(temporaryPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			returnErr = errors.Join(returnErr, serveOperationFailure("could not remove temporary serve Inbox file", removeErr))
		} else if removeErr == nil {
			if syncErr := syncDirectory(directory); syncErr != nil {
				returnErr = errors.Join(returnErr, syncErr)
			}
		}
	}()
	if err := temporary.Chmod(openedInfo.Mode().Perm() & 0o666); err != nil {
		return InboxCopy{}, serveOperationFailure("could not set permissions on temporary serve Inbox file", err)
	}
	hash := sha256.New()
	written, err := copyContext(ctx, io.MultiWriter(temporary, hash), input)
	if err != nil {
		return InboxCopy{}, serveOperationFailure("could not copy selected serve source to Inbox", err)
	}
	if written != openedInfo.Size() {
		return InboxCopy{}, ErrSourceChanged
	}
	if err := source.Revalidate(); err != nil {
		return InboxCopy{}, serveOperationFailure("selected serve source changed or became unavailable", ErrSourceChanged, err)
	}
	if err := input.Close(); err != nil {
		inputOpen = false
		return InboxCopy{}, serveOperationFailure("could not close selected serve source", err)
	}
	inputOpen = false
	if err := temporary.Sync(); err != nil {
		return InboxCopy{}, serveOperationFailure("could not sync temporary serve Inbox file", err)
	}
	if err := temporary.Close(); err != nil {
		temporaryOpen = false
		return InboxCopy{}, serveOperationFailure("could not close temporary serve Inbox file", err)
	}
	temporaryOpen = false
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	verifiedSize, verifiedHash, err := verify(temporaryPath)
	if err != nil || verifiedSize != written || verifiedHash != digest {
		return InboxCopy{}, serveOperationFailure("temporary serve Inbox file failed verification", ErrSourceChanged, err)
	}
	finalPath, err := publishInboxFile(temporaryPath, directory, filepath.Base(source.Path))
	if err != nil {
		return InboxCopy{}, err
	}
	result = InboxCopy{Path: finalPath, Size: written, SHA256: digest}
	defer func() {
		if returnErr != nil {
			if removeErr := os.Remove(finalPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				returnErr = errors.Join(returnErr, serveOperationFailure("could not roll back incomplete serve Inbox publication", removeErr))
			}
			if syncErr := syncDirectory(directory); syncErr != nil {
				returnErr = errors.Join(returnErr, syncErr)
			}
		}
	}()
	if err := syncDirectory(directory); err != nil {
		return InboxCopy{}, err
	}
	return result, nil
}

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 128*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			cause := context.Cause(ctx)
			if cause == nil {
				cause = err
			}
			return written, cause
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			count, writeErr := destination.Write(buffer[:read])
			written += int64(count)
			if writeErr != nil {
				return written, writeErr
			}
			if count != read {
				return written, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}

func publishInboxFile(temporaryPath, directory, originalName string) (string, error) {
	for attempt := 0; attempt < maxInboxNameAttempts; attempt++ {
		finalPath := inboxAttemptPath(directory, originalName, attempt)
		if err := os.Link(temporaryPath, finalPath); err == nil {
			if err := os.Remove(temporaryPath); err != nil {
				rollbackErr := os.Remove(finalPath)
				if errors.Is(rollbackErr, os.ErrNotExist) {
					rollbackErr = nil
				}
				syncErr := syncDirectory(directory)
				return "", errors.Join(
					serveOperationFailure("could not complete serve Inbox publication", err),
					serveOperationFailure("could not roll back incomplete serve Inbox publication", rollbackErr),
					syncErr,
				)
			}
			return finalPath, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", serveOperationFailure("could not publish file to selected serve Inbox", err)
		}
	}
	return "", ErrInboxCollision
}

func availableInboxPath(directory, originalName string) (string, error) {
	for attempt := 0; attempt < maxInboxNameAttempts; attempt++ {
		candidate := inboxAttemptPath(directory, originalName, attempt)
		if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		} else if err != nil {
			return "", serveOperationFailure("could not inspect selected serve Inbox", err)
		}
	}
	return "", ErrInboxCollision
}

func inboxAttemptPath(directory, originalName string, attempt int) string {
	extension := filepath.Ext(originalName)
	stem := strings.TrimSuffix(originalName, extension)
	name := originalName
	if attempt > 0 {
		name = fmt.Sprintf("%s-%d%s", stem, attempt, extension)
	}
	return filepath.Join(directory, name)
}

func hashFile(name string) (int64, [sha256.Size]byte, error) {
	file, err := os.Open(name)
	if err != nil {
		return 0, [sha256.Size]byte{}, err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	closeErr := file.Close()
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return size, digest, errors.Join(
		serveOperationFailure("could not read temporary serve Inbox file", err),
		serveOperationFailure("could not close temporary serve Inbox file", closeErr),
	)
}
