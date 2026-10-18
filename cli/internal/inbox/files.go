package inbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// ScanLimit bounds the directory entries read, so a huge folder cannot stall a caller.
	ScanLimit = 5000
	// FileIDLength is the length of a file id (hex of the first 8 bytes of the name's SHA-256).
	FileIDLength = 16
)

// ErrFileNotFound means no delivered file has this id.
var ErrFileNotFound = errors.New("inbox file not found")

// File is one delivered file in the Inbox folder.
type File struct {
	ID         string
	Name       string
	Size       int64
	ReceivedAt time.Time
}

// FileID is stable for a file name, so a listing and a later read or delete agree on the file.
func FileID(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:8])
}

// ValidFileID reports whether id has the shape FileID produces.
func ValidFileID(id string) bool {
	if len(id) != FileIDLength {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && id == strings.ToLower(id)
}

func deliveredName(name string) bool {
	return name != "" && !strings.HasPrefix(name, ".") && len(name) <= 255 && utf8.ValidString(name) && !strings.ContainsAny(name, "/\\\x00")
}

// Files lists the regular files delivered to the Inbox folder, newest first, and how many there are
// (the scan stops at ScanLimit entries). Hidden files (the receipt journal and partial downloads),
// folders and links are not Inbox files.
func Files(root string) ([]File, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	files := make([]File, 0, len(entries))
	for index, entry := range entries {
		if index >= ScanLimit {
			break
		}
		name := entry.Name()
		if !deliveredName(name) || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, File{ID: FileID(name), Name: name, Size: info.Size(), ReceivedAt: info.ModTime().UTC()})
	}
	sort.Slice(files, func(i, j int) bool {
		if !files[i].ReceivedAt.Equal(files[j].ReceivedAt) {
			return files[i].ReceivedAt.After(files[j].ReceivedAt)
		}
		return files[i].Name < files[j].Name
	})
	return files, nil
}

// Find resolves a file id to the delivered file's name by looking at the folder, never at a path the
// caller supplied.
func Find(root, id string) (File, error) {
	if !ValidFileID(id) {
		return File{}, ErrFileNotFound
	}
	files, err := Files(root)
	if err != nil {
		return File{}, err
	}
	for _, file := range files {
		if file.ID == id {
			return file, nil
		}
	}
	return File{}, ErrFileNotFound
}

// Open opens a delivered file for reading. The link check and the open use the same descriptor, so a
// file swapped for a link after the listing is refused.
func Open(root, id string) (*os.File, File, error) {
	file, err := Find(root, id)
	if err != nil {
		return nil, File{}, err
	}
	path := filepath.Join(root, file.Name)
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return nil, File{}, ErrFileNotFound
	}
	handle, err := os.Open(path)
	if err != nil {
		return nil, File{}, ErrFileNotFound
	}
	info, err := handle.Stat()
	if err != nil || !info.Mode().IsRegular() {
		handle.Close()
		return nil, File{}, ErrFileNotFound
	}
	file.Size, file.ReceivedAt = info.Size(), info.ModTime().UTC()
	return handle, file, nil
}

// Remove deletes one delivered file; the receipt journal is left as it is.
func Remove(root, id string) error {
	file, err := Find(root, id)
	if err != nil {
		return err
	}
	path := filepath.Join(root, file.Name)
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return ErrFileNotFound
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrFileNotFound
		}
		return err
	}
	return nil
}
