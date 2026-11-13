package tlscert

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// A restarted edge process holds no certificates: the server sends each (node, epoch) its actions once and wipes
// the key material when they are acknowledged, so after a deploy every TLS handshake waited for the server to stage
// the new epoch's certificates (18 to 109 s on CI, w28 run 43078027365). The registry therefore writes each active
// certificate to the edge's private state directory and a starting process restores them before it serves; the
// server's own delivery for the new epoch then replaces them through the usual generation fences. Prior art:
// tailscale feature/acme/certstore.go (certFileStore keeps certs and keys in files, 0600 in a 0700 directory, and a
// restarted tailscaled serves them at once) and caddy/certmagic FileStorage (atomic temp-and-rename writes).
// The edge has no at-rest sealing key, so the files rely on the 0600 mode like the node's other durable secrets.

const (
	maxPersistedRecordBytes = 1 << 20
	maxPersistedTotalBytes  = 64 << 20 // everything Load reads: far above any real edge, below a memory hazard
	persistedSuffix         = ".cert"
	persistedTemporary      = ".tmp-"
)

// ErrUnsafeStore reports a certificate directory or file that other users could read.
var ErrUnsafeStore = errors.New("certificate store permissions are unsafe")

// Persistence stores one record per host key. Save replaces the record; Delete of a missing record is not an error.
type Persistence interface {
	Save(key string, record []byte) error
	Delete(key string) error
}

// DiskStore keeps records as files named after the SHA-256 of the host key.
type DiskStore struct{ dir string }

// NewDiskStore creates dir (mode 0700) when it is missing and refuses one that group or others can use.
func NewDiskStore(dir string) (*DiskStore, error) {
	if dir == "" || strings.ContainsRune(dir, 0) {
		return nil, fmt.Errorf("%w: certificate directory is empty", ErrInvalid)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create certificate directory: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("stat certificate directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: %s", ErrUnsafeStore, dir)
	}
	return &DiskStore{dir: dir}, nil
}

func (s *DiskStore) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:])+persistedSuffix)
}

// Save writes the record to a temporary file, syncs it and renames it over the previous record, so a crash leaves
// either the old or the new record and never a partial one.
func (s *DiskStore) Save(key string, record []byte) error {
	if len(record) == 0 || len(record) > maxPersistedRecordBytes {
		return fmt.Errorf("%w: certificate record size", ErrInvalid)
	}
	temporary, err := os.CreateTemp(s.dir, persistedTemporary+"*")
	if err != nil {
		return fmt.Errorf("create certificate temporary file: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name) // a no-op once the rename succeeded
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set certificate file permissions: %w", err)
	}
	if _, err := temporary.Write(record); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write certificate file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync certificate file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close certificate file: %w", err)
	}
	if err := os.Rename(name, s.path(key)); err != nil {
		return fmt.Errorf("replace certificate file: %w", err)
	}
	directory, err := os.Open(s.dir) // the rename itself must survive a crash, as in edge/internal/store
	if err != nil {
		return fmt.Errorf("open certificate directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync certificate directory: %w", err)
	}
	return nil
}

// Delete removes the record of key.
func (s *DiskStore) Delete(key string) error {
	if err := os.Remove(s.path(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete certificate file: %w", err)
	}
	return nil
}

// Load returns every record file, and removes the leftovers of an interrupted Save. A file that is not a regular
// 0600 file, or is too large, is skipped and counted in unsafe (it is never read). The caller wipes the records.
func (s *DiskStore) Load() (records [][]byte, unsafe int, err error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, 0, fmt.Errorf("read certificate directory: %w", err)
	}
	total := 0
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case strings.HasPrefix(name, persistedTemporary):
			_ = os.Remove(filepath.Join(s.dir, name))
			continue
		case !strings.HasSuffix(name, persistedSuffix):
			continue
		}
		record, ok := readPrivateFile(filepath.Join(s.dir, name))
		if !ok || total+len(record) > maxPersistedTotalBytes {
			clear(record)
			unsafe++
			continue
		}
		total += len(record)
		records = append(records, record)
	}
	return records, unsafe, nil
}

// readPrivateFile reads a regular 0600 file. Lstat comes first, as in edge/internal/store: a symlink or a FIFO
// planted in the directory is never opened (opening a FIFO would block the start forever).
func readPrivateFile(path string) ([]byte, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > maxPersistedRecordBytes {
		return nil, false
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	record, err := io.ReadAll(io.LimitReader(file, maxPersistedRecordBytes+1))
	if err != nil || len(record) > maxPersistedRecordBytes {
		clear(record)
		return nil, false
	}
	return record, true
}

// persistJob is a write or a delete decided under the registry lock and done after it is released, so a slow disk
// never holds up a TLS handshake.
type persistJob struct {
	key        string
	bindingKey string // the entry a save is for
	record     []byte
	remove     bool
	encodeErr  error
}

func (r *Registry) persistJobLocked(key string, e *entry) *persistJob {
	if r.persist == nil || e.restored { // a restored entry is already on disk
		return nil
	}
	record, err := encodeEntry(e)
	return &persistJob{key: key, bindingKey: bindingKey(e.binding), record: record, encodeErr: err}
}

// finishPersist runs the job unless the registry has moved on since it was decided: jobs of one host key can finish
// out of order, and a save of an older certificate (or a delete after a newer activation) must not overwrite the
// newer file. persistMu makes the check and the write one step.
func (r *Registry) finishPersist(job *persistJob) {
	if job == nil || r.persist == nil {
		return
	}
	defer clear(job.record)
	r.persistMu.Lock()
	defer r.persistMu.Unlock()
	r.mu.RLock()
	current := r.active[job.key]
	r.mu.RUnlock()
	if job.remove && current != "" || !job.remove && current != job.bindingKey {
		return
	}
	var err error
	operation := "save"
	switch {
	case job.encodeErr != nil:
		operation, err = "encode", job.encodeErr
	case job.remove:
		operation, err = "delete", r.persist.Delete(job.key)
	default:
		err = r.persist.Save(job.key, job.record)
	}
	if err != nil && r.onError != nil {
		r.onError(operation, err)
	}
}

// RestoreEncoded stages, readies and activates the certificates a previous process of this node persisted, through
// the registry's own checks (chain and key match, hostname, lifetime). They carry the previous process's epoch, so
// the server's delivery for this epoch is an ordinary replacement: the same certificate replays through the
// generation fences, a newer one supersedes it, and the activation discards the restored entry. A record that
// fails a check is dropped and counted; an expired or invalid one is also deleted. The restore never fails the
// start. The records are wiped before it returns.
func (r *Registry) RestoreEncoded(ctx context.Context, nodeID string, records [][]byte) ImportReport {
	defer WipeEncoded(records)
	report := ImportReport{Dropped: map[string]int{}}
	for _, record := range records {
		var item ExportedCertificate
		if json.Unmarshal(record, &item) != nil {
			report.Dropped["malformed"]++
			continue
		}
		reason := r.restoreOne(ctx, nodeID, item)
		clear(item.Bundle.PrivateKeyPEM)
		clear(item.Bundle.CertificatePEM)
		if reason == "" {
			report.Imported++
			continue
		}
		report.Dropped[reason]++
		if r.persist != nil && (reason == "expired" || reason == "invalid" || reason == "other_node") {
			if err := r.persist.Delete(hostKey(item.Binding.Hostname, strings.HasPrefix(item.Binding.Hostname, "*."))); err != nil && r.onError != nil {
				r.onError("delete", err)
			}
		}
	}
	return report
}

func (r *Registry) restoreOne(ctx context.Context, nodeID string, item ExportedCertificate) string {
	if item.Binding.EdgeNodeID != nodeID {
		return "other_node"
	}
	if err := r.stage(ctx, item.Binding, item.Bundle, nil); err != nil {
		return importDropReason(err)
	}
	r.mu.Lock()
	if e := r.entries[bindingKey(item.Binding)]; e != nil {
		e.restored = true
	}
	r.mu.Unlock()
	for _, step := range []func() error{
		func() error { return r.markReady(ctx, item.Binding, nil) },
		func() error { return r.activate(ctx, item.Binding, nil) },
	} {
		if err := step(); err != nil {
			return importDropReason(err)
		}
	}
	return ""
}
