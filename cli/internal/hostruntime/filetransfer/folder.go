package filetransfer

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pinksaucepasta/paperboat/cli/internal/transfername"
)

// A folder arrives as one transfer holding a tar stream (see internal/filetransfer/folder.go). The receiver stores it as
// a folder of the transfer's name in the Inbox: folders and regular files only, every name held to the rules of a single
// received file, no entry before its folder, no duplicates, files 0600 and folders 0700 like every Inbox file (the
// sender's modes are ignored, so nothing arrives executable or setuid). It is checked whole when the upload completes and
// unpacked into a hidden folder that is renamed into place, so the Inbox never shows half a folder.

func (s *Service) directoryMarker(id string) string { return filepath.Join(s.config.Root, id+".dir") }

func (s *Service) isDirectory(id string) bool {
	info, err := os.Lstat(s.directoryMarker(id))
	return err == nil && info.Mode().IsRegular()
}

// folderEntryLimit is replaced by tests to avoid making a hundred thousand files.
var folderEntryLimit = transfername.MaxFolderEntries

// createUnpacked is replaced by tests to stand in for a disk that fills up part way through a folder.
var createUnpacked = func(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

func folderRefusal(path, reason string) error {
	return &Error{Code: InvalidPath, Cause: fmt.Errorf("%s: %s", path, reason)}
}

// walkFolder checks every entry of a folder tar and hands each folder and regular file to visit.
func walkFolder(content io.Reader, platform string, visit func(rel string, dir bool, size int64, data io.Reader) error) error {
	archive := tar.NewReader(content)
	seen := map[string]bool{} // path (lower-cased where the receiver ignores case) -> is a folder
	key := func(rel string) string {
		if platform == "windows" || platform == "darwin" {
			return strings.ToLower(rel)
		}
		return rel
	}
	for count := 1; ; count++ {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return folderRefusal("the folder", "its archive is damaged: "+err.Error())
		}
		rel := strings.TrimSuffix(header.Name, "/")
		dir := header.Typeflag == tar.TypeDir
		switch {
		case !dir && header.Typeflag != tar.TypeReg:
			return folderRefusal(rel, "only folders and regular files are stored, not links or special files")
		case count > folderEntryLimit:
			return folderRefusal(rel, fmt.Sprintf("the folder holds more than %d files and folders", folderEntryLimit))
		case header.Size < 0 || dir && header.Size != 0:
			return folderRefusal(rel, "invalid size")
		}
		if reason := transfername.FolderPathRefusal(platform, rel); reason != "" {
			return folderRefusal(rel, reason)
		}
		if seen[key(rel)] || seen[key(rel)+"/"] {
			return folderRefusal(rel, "the name appears twice (names are not case-sensitive on this machine)")
		}
		if i := strings.LastIndexByte(rel, '/'); i >= 0 && !seen[key(rel[:i])+"/"] {
			return folderRefusal(rel, "it comes before its folder")
		}
		if dir {
			seen[key(rel)+"/"] = true
		} else {
			seen[key(rel)] = true
		}
		if err := visit(rel, dir, header.Size, archive); err != nil {
			return err
		}
	}
}

// checkFolder refuses a folder the receiver cannot store, before it is published.
func (s *Service) checkFolder(content string) error {
	file, err := os.Open(content)
	if err != nil {
		return storageFailure(err)
	}
	defer file.Close()
	return walkFolder(file, s.config.Platform, func(string, bool, int64, io.Reader) error { return nil })
}

// unpackFolder writes the folder tar into the new folder dest. The caller removes dest on failure.
func (s *Service) unpackFolder(content, dest string) error {
	file, err := os.Open(content)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := os.Mkdir(dest, 0o700); err != nil {
		return err
	}
	return walkFolder(file, s.config.Platform, func(rel string, dir bool, size int64, data io.Reader) error {
		path := filepath.Join(dest, filepath.FromSlash(rel))
		if dir {
			return os.Mkdir(path, 0o700)
		}
		out, err := createUnpacked(path)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(out, data, size)
		return errors.Join(copyErr, out.Close())
	})
}

// unpackToInbox publishes a folder transfer: unpack beside the final name, then rename into place without replacing
// anything (ErrExist means a name taken, as for a file). The spool keeps the tar until the folder is in place, so a failed
// attempt (full disk) removes its partial folder and a repeated complete starts again.
func (s *Service) unpackToInbox(content, root, path, id string) error {
	temporary := filepath.Join(root, "."+id+".publish")
	if err := os.RemoveAll(temporary); err != nil {
		return err
	}
	if err := s.unpackFolder(content, temporary); err != nil {
		return errors.Join(err, os.RemoveAll(temporary))
	}
	if err := renameNoReplace(temporary, path); err != nil {
		return errors.Join(err, os.RemoveAll(temporary))
	}
	return removeIfExists(content)
}
