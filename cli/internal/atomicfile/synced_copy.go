package atomicfile

import "io"

// SyncBytes bounds how much unwritten data one large copy leaves to the kernel.
// One unsynced 90 MB binary copy saturated dadape's SD card for ~10 s, and
// processes faulting in their own pages (session owners, hostd) stalled 5-6 s,
// pausing every running terminal (direct2c, direct1d). dpkg bounds writeback
// the same way (sync_file_range after each file).
const SyncBytes = 8 << 20

// WriteSyncer is a destination that can flush its written data to disk.
type WriteSyncer interface {
	io.Writer
	Sync() error
}

// SyncedCopy copies src to dst, syncing every SyncBytes so writeback stays
// short. The caller still syncs the tail.
func SyncedCopy(dst WriteSyncer, src io.Reader) (int64, error) {
	var written int64
	for {
		n, err := io.CopyN(dst, src, SyncBytes)
		written += n
		if err == io.EOF {
			return written, nil
		}
		if err != nil {
			return written, err
		}
		if err := dst.Sync(); err != nil {
			return written, err
		}
	}
}
