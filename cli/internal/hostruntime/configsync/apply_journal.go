package configsync

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var ErrApplyJournalInvalid = errors.New("invalid config apply journal")

type applyJournalEntry struct {
	Path        string      `json:"path"`
	Destination string      `json:"destination,omitempty"`
	Existed     bool        `json:"existed"`
	Mode        os.FileMode `json:"mode,omitempty"`
	Target      string      `json:"target,omitempty"`
	Content     []byte      `json:"content,omitempty"`
}

type applyJournal struct {
	Format          string              `json:"format"`
	MappingRevision string              `json:"mapping_revision,omitempty"`
	MappingRules    []PathRule          `json:"mapping_rules,omitempty"`
	RepositoryID    string              `json:"repository_id"`
	AssignmentID    string              `json:"assignment_id"`
	RemoteRevision  string              `json:"remote_revision"`
	Entries         []applyJournalEntry `json:"entries"`
}

func beginApplyJournal(path, homeRoot, repositoryID, assignmentID, remoteRevision string, paths []string, maxBytes int64, mappings ...*PathMapping) error {
	if !canonicalAbsolutePath(path) || !canonicalAbsolutePath(homeRoot) || repositoryID == "" ||
		assignmentID == "" || remoteRevision == "" || maxBytes <= 0 {
		return ErrApplyJournalInvalid
	}
	paths = append([]string(nil), paths...)
	sort.Strings(paths)
	paths = deduplicatePaths(paths)
	journal := applyJournal{
		Format: "paperboat-config-apply-journal-v1", RepositoryID: repositoryID,
		AssignmentID: assignmentID, RemoteRevision: remoteRevision,
		Entries: make([]applyJournalEntry, 0, len(paths)),
	}
	if len(mappings) > 0 {
		journal.MappingRevision = mappings[0].Revision()
		journal.MappingRules = append([]PathRule(nil), mappings[0].rules...)
	}
	var total int64
	for _, relative := range paths {
		if !safeRelativeStatusPath(relative) {
			return ErrApplyJournalInvalid
		}
		full := filepath.Join(homeRoot, filepath.FromSlash(relative))
		if len(mappings) > 0 {
			var ok bool
			full, ok = mappings[0].LocalPath(relative)
			if !ok {
				return ErrApplyJournalInvalid
			}
			if err := checkSafeAbsolutePath(full); err != nil {
				return err
			}
		}
		if len(mappings) == 0 && !sameOrInsidePath(full, homeRoot) {
			return ErrApplyJournalInvalid
		}
		entry := applyJournalEntry{Path: relative}
		if len(mappings) > 0 {
			entry.Destination = full
		}
		info, err := os.Lstat(full)
		if errors.Is(err, os.ErrNotExist) {
			journal.Entries = append(journal.Entries, entry)
			continue
		}
		if err != nil || info.IsDir() || info.Mode()&(os.ModeDevice|os.ModeNamedPipe|os.ModeSocket) != 0 {
			return errors.Join(ErrApplyJournalInvalid, err)
		}
		entry.Existed, entry.Mode = true, info.Mode()
		if info.Mode()&os.ModeSymlink != 0 {
			entry.Target, err = os.Readlink(full)
			if err != nil || filepath.IsAbs(entry.Target) {
				return errors.Join(ErrApplyJournalInvalid, err)
			}
		} else {
			if !info.Mode().IsRegular() || info.Size() > maxBytes-total {
				return ErrApplyJournalInvalid
			}
			var opened os.FileInfo
			entry.Content, opened, err = secureReadFile(full, maxBytes-total)
			if err == nil && !os.SameFile(info, opened) {
				return ErrSourceChanged
			}
			if err != nil {
				return err
			}
			total += int64(len(entry.Content))
		}
		journal.Entries = append(journal.Entries, entry)
	}
	plaintext, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	if int64(len(plaintext)) > encodedApplyJournalLimit(maxBytes) {
		return ErrApplyJournalInvalid
	}
	return writePrivateAtomic(path, append(plaintext, '\n'))
}

func recoverApplyJournal(path, homeRoot, repositoryID, assignmentID string, maxBytes int64, mappings ...*PathMapping) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	maxJournalBytes := encodedApplyJournalLimit(maxBytes)
	if err != nil || !privateControlFile(path, info) || info.Size() > maxJournalBytes {
		return errors.Join(ErrApplyJournalInvalid, err)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	var journal applyJournal
	decoder := json.NewDecoder(io.LimitReader(file, maxJournalBytes))
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&journal)
	trailingErr := decoder.Decode(&struct{}{})
	closeErr := file.Close()
	if closeErr != nil {
		return closeErr
	}
	if decodeErr != nil || !errors.Is(trailingErr, io.EOF) ||
		journal.Format != "paperboat-config-apply-journal-v1" ||
		journal.RepositoryID != repositoryID || journal.AssignmentID != assignmentID {
		return ErrApplyJournalInvalid
	}
	if !validApplyJournalEntries(journal.Entries, maxBytes) {
		return ErrApplyJournalInvalid
	}
	if len(mappings) > 0 && mappings[0] == nil {
		mappings = nil
	}
	if len(mappings) == 0 && journal.MappingRevision != "" {
		original, err := ResolvePathRules(homeRoot, journal.MappingRules)
		if err != nil || original.Revision() != journal.MappingRevision {
			return ErrApplyJournalInvalid
		}
		mappings = []*PathMapping{original}
	}
	if len(mappings) > 0 && journal.MappingRevision != mappings[0].Revision() {
		previous, err := ResolvePathRules(homeRoot, journal.MappingRules)
		if err != nil || previous.Revision() != journal.MappingRevision {
			return ErrApplyJournalInvalid
		}
		mappings = []*PathMapping{previous}
	}
	for _, entry := range journal.Entries {
		if len(mappings) > 0 {
			target, ok := mappings[0].LocalPath(entry.Path)
			if !ok || target != entry.Destination {
				return ErrApplyJournalInvalid
			}
			if err := checkSafeAbsolutePath(target); err != nil {
				return err
			}
		} else if entry.Destination != "" {
			return ErrApplyJournalInvalid
		}
	}
	for index := len(journal.Entries) - 1; index >= 0; index-- {
		if err := restoreApplyJournalEntry(homeRoot, journal.Entries[index], mappings...); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

// encodedApplyJournalLimit accounts for JSON's base64 encoding of each
// applyJournalEntry.Content value. The raw rollback content is limited to
// maxBytes by beginApplyJournal; recovery must accept the corresponding valid
// encoded journal while retaining a bounded parser input.
func encodedApplyJournalLimit(maxBytes int64) int64 {
	if maxBytes <= 0 {
		return 0
	}
	return maxBytes + (maxBytes+2)/3 + (1 << 20)
}

func validApplyJournalEntries(entries []applyJournalEntry, maxBytes int64) bool {
	if maxBytes <= 0 {
		return false
	}
	var total int64
	for _, entry := range entries {
		if !safeRelativeStatusPath(entry.Path) {
			return false
		}
		if entry.Mode&os.ModeSymlink != 0 {
			if !entry.Existed || entry.Target == "" || filepath.IsAbs(entry.Target) || len(entry.Content) != 0 {
				return false
			}
			continue
		}
		if !entry.Existed {
			if entry.Mode != 0 || entry.Target != "" || len(entry.Content) != 0 {
				return false
			}
			continue
		}
		if !entry.Mode.IsRegular() || entry.Target != "" || int64(len(entry.Content)) > maxBytes-total {
			return false
		}
		total += int64(len(entry.Content))
	}
	return true
}

func restoreApplyJournalEntry(homeRoot string, entry applyJournalEntry, mappings ...*PathMapping) error {
	if !safeRelativeStatusPath(entry.Path) {
		return ErrApplyJournalInvalid
	}
	target := filepath.Join(homeRoot, filepath.FromSlash(entry.Path))
	if len(mappings) > 0 {
		var ok bool
		target, ok = mappings[0].LocalPath(entry.Path)
		if !ok || target != entry.Destination {
			return ErrApplyJournalInvalid
		}
		homeRoot = filepath.VolumeName(target) + string(filepath.Separator)
	}
	if !entry.Existed {
		return secureRemoveFile(target)
	}
	if !entry.Mode.IsRegular() {
		return ErrApplyJournalInvalid
	}
	return secureWriteFile(target, entry.Content, entry.Mode.Perm())
}

func ensurePrivateParent(root, parent string) error {
	relative, err := filepath.Rel(root, parent)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return ErrApplyJournalInvalid
	}
	current := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			if mkdirErr := os.Mkdir(current, 0o700); mkdirErr != nil {
				return mkdirErr
			}
			continue
		}
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.Join(ErrApplyJournalInvalid, statErr)
		}
	}
	return nil
}

func deduplicatePaths(paths []string) []string {
	result := paths[:0]
	for _, path := range paths {
		if len(result) == 0 || result[len(result)-1] != path {
			result = append(result, path)
		}
	}
	return result
}
