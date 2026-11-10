package windowsopenssh

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
)

const (
	// serviceLogCap bounds a generation's service.log; at the cap the last serviceLogKeep bytes stay (whole lines), so
	// the sessions still open keep their "Accepted publickey" lines for the per-key revoke.
	serviceLogCap  = 16 << 20
	serviceLogKeep = 8 << 20
)

// cappedLog is the writer for sshd's stderr. It never fails a write: sshd must not block on a full pipe, and an
// unwritten log only makes the per-key revoke fall back to ending every session.
type cappedLog struct {
	mu   sync.Mutex
	file *os.File // opened read-write, not append: compaction truncates it
	size int64
}

func newCappedLog(file *os.File) (*cappedLog, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	return &cappedLog{file: file, size: info.Size()}, nil
}

func (l *cappedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size+int64(len(p)) > serviceLogCap {
		_ = l.compact()
	}
	if n, err := l.file.WriteAt(p, l.size); err == nil {
		l.size += int64(n)
	}
	return len(p), nil
}

func (l *cappedLog) compact() error {
	keep := min(l.size, serviceLogKeep)
	tail := make([]byte, keep)
	if _, err := l.file.ReadAt(tail, l.size-keep); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if i := bytes.IndexByte(tail, '\n'); i >= 0 && i+1 < len(tail) {
		tail = tail[i+1:]
	}
	if err := l.file.Truncate(0); err != nil {
		return err
	}
	n, err := l.file.WriteAt(tail, 0)
	l.size = int64(n)
	return err
}
