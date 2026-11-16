package filetransfer

import (
	"archive/tar"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/transfername"
)

// A folder is sent as one transfer whose bytes are a tar stream of its files and folders, written while it is sent
// (no temporary copy) and unpacked by the receiver into a folder of the same name in its Inbox. One transfer means the
// whole existing path applies unchanged: approval, the size limits of the transfer mode (on the total), resume from the
// committed offset, digest check and all-or-nothing publication. Prior art: magic-wormhole sends a folder as one zip
// (src/wormhole/cli/cmd_send.py make_zipfile) and croc zips it too (--zip); rsync skips what is not a regular file
// ("skipping non-regular file", flist.c) unless asked to copy links. Taildrop sends files only.

// SkippedEntry is something inside a folder that is not sent: links and special files, the same on every OS.
type SkippedEntry struct {
	Path string // slash-separated, relative to the folder
	Kind string // "symbolic link" or "special file"
}

// Folder describes a folder source: what the receiver will be asked to store, and what was left out.
type Folder struct {
	Paths   []string // slash-separated path of every file and folder inside, in sending order
	Files   int
	Skipped []SkippedEntry
}

// FolderEntriesError is a folder with more files and folders than one send carries. Nothing was sent.
type FolderEntriesError struct {
	Path  string
	Limit int
}

func (e *FolderEntriesError) Error() string {
	return fmt.Sprintf("%s holds more than %d files and folders", e.Path, e.Limit)
}

// folderEntryLimit is replaced by tests to avoid making a hundred thousand files.
var folderEntryLimit = transfername.MaxFolderEntries

// SourceChangedError is a file inside a folder that was replaced, shrank or grew while the folder was being sent.
// Nothing is delivered; sending again describes the folder as it is then.
type SourceChangedError struct{ Path string }

func (e *SourceChangedError) Error() string { return e.Path + " changed while sending" }

type folderEntry struct {
	rel  string
	dir  bool
	size int64
}

// scanFolder lists what a folder holds in a fixed order (directories before their contents, names sorted), so the same
// folder always streams the same bytes, which resume relies on.
func scanFolder(root string) ([]folderEntry, *Folder, error) {
	var entries []folderEntry
	folder := &Folder{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(relative)
		switch {
		case d.IsDir():
			entries = append(entries, folderEntry{rel: rel, dir: true})
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			entries = append(entries, folderEntry{rel: rel, size: info.Size()})
			folder.Files++
		case d.Type()&fs.ModeSymlink != 0:
			folder.Skipped = append(folder.Skipped, SkippedEntry{Path: rel, Kind: "symbolic link"})
			return nil
		default:
			folder.Skipped = append(folder.Skipped, SkippedEntry{Path: rel, Kind: "special file"})
			return nil
		}
		if len(entries) > folderEntryLimit {
			return &FolderEntriesError{Path: root, Limit: folderEntryLimit}
		}
		folder.Paths = append(folder.Paths, rel)
		return nil
	})
	return entries, folder, err
}

// folderStream is the tar of a folder as a ReadSeeker: reading starts a writer that walks the entries once; seeking
// forward discards, seeking back starts over. Resume seeks to the committed offset, which costs one read of the data
// before it (what the hashing pass already paid), never a copy on disk.
type folderStream struct {
	root    string
	entries []folderEntry
	pos     int64
	reader  *io.PipeReader
}

func (s *folderStream) Read(p []byte) (int, error) {
	if s.reader == nil {
		reader, writer := io.Pipe()
		s.reader = reader
		go func() { writer.CloseWithError(writeFolderTar(writer, s.root, s.entries)) }()
	}
	n, err := s.reader.Read(p)
	s.pos += int64(n)
	return n, err
}

func (s *folderStream) Seek(offset int64, whence int) (int64, error) {
	if whence != io.SeekStart || offset < 0 {
		return 0, errors.New("folder stream: only seeking from the start is supported")
	}
	if offset < s.pos {
		_ = s.Close()
	}
	if offset > s.pos {
		if _, err := io.CopyN(io.Discard, s, offset-s.pos); err != nil {
			return s.pos, err
		}
	}
	return s.pos, nil
}

func (s *folderStream) Close() error {
	if s.reader != nil {
		_ = s.reader.CloseWithError(io.ErrClosedPipe)
		s.reader = nil
	}
	s.pos = 0
	return nil
}

// folderModTime is fixed: the receiver does not keep modification times (a single file does not either), and a fixed
// value keeps the stream identical from one pass to the next.
var folderModTime = time.Unix(0, 0)

func writeFolderTar(w io.Writer, root string, entries []folderEntry) error {
	archive := tar.NewWriter(w)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.rel, ModTime: folderModTime, Typeflag: tar.TypeReg, Mode: 0o644, Size: entry.size}
		if entry.dir {
			header.Name, header.Typeflag, header.Mode, header.Size = entry.rel+"/", tar.TypeDir, 0o755, 0
		}
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		if !entry.dir {
			if err := copyFolderFile(archive, filepath.Join(root, filepath.FromSlash(entry.rel)), entry.size); err != nil {
				return err
			}
		}
	}
	return archive.Close()
}

// copyFolderFile writes exactly size bytes of a regular file; a file that was replaced, shrank or grew since the scan
// fails the send instead of sending bytes the manifest does not describe.
func copyFolderFile(w io.Writer, path string, size int64) error {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return &SourceChangedError{Path: path}
	}
	file, err := openSourceFile(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || opened.Size() != size {
		return &SourceChangedError{Path: path}
	}
	if _, err := io.CopyN(w, file, size); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}

// prepareFolder opens a folder for sending: it scans it, hashes the tar stream once (bounded by the file limit, since the
// folder is one transfer) and leaves a stream positioned at the start.
func prepareFolder(path string, limits Limits) (Source, error) {
	name := filepath.Base(path)
	if filepath.Dir(path) == path {
		return Source{}, fmt.Errorf("%s is a drive or file system root; send a folder inside it", path)
	}
	entries, folder, err := scanFolder(path)
	if err != nil {
		var tooMany *FolderEntriesError
		if errors.As(err, &tooMany) {
			return Source{}, err
		}
		return Source{}, fmt.Errorf("read folder %s: %w", path, err)
	}
	var content int64
	for _, entry := range entries {
		content += entry.size
	}
	if content > limits.MaxFileBytes {
		return Source{}, &LimitError{Scope: LimitFileBytes, Name: name, Value: content, Limit: limits.MaxFileBytes}
	}
	stream := &folderStream{root: path, entries: entries}
	hash := sha256.New()
	read, err := io.CopyBuffer(hash, io.LimitReader(stream, limits.MaxFileBytes+1), make([]byte, 32<<10))
	if err == nil && read > limits.MaxFileBytes {
		err = &LimitError{Scope: LimitFileBytes, Name: name, Value: read, Limit: limits.MaxFileBytes}
	}
	if err != nil {
		_ = stream.Close()
		return Source{}, err
	}
	if _, err := stream.Seek(0, io.SeekStart); err != nil {
		return Source{}, err
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return Source{Basename: name, Size: read, SHA256: digest, Reader: stream, Folder: folder}, nil
}
