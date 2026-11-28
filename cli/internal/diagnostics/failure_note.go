package diagnostics

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
)

// FailureNoteName holds why the last pb process that could not finish saving gave up.
const FailureNoteName = "last-diagnostics-failure.txt"

// NoteSaveFailure replaces the failure note in the ring's directory. It needs no writer.lock, because another holder
// of the lock is the usual reason to write it, and it never syncs: a disk too slow to save the ring must not hold the
// exit for another sync. The file is created like a segment (owner-only, never through a link) and renamed into
// place, so two exiting processes cannot mix their notes. A memory recorder keeps no note.
func (r *Recorder) NoteSaveFailure(note string) error {
	if r == nil || r.disk == nil {
		return nil
	}
	return r.disk.writeNote([]byte(note))
}

func (r *DiskRing) writeNote(note []byte) (result error) {
	// Verify, never create: a directory an uninstall removed stays removed.
	if err := verifyDiagnosticDirectory(r.config.Directory, r.owner); err != nil {
		return err
	}
	// A random name: a file left by a killed process, or another caller, never blocks this one.
	pending := filepath.Join(r.config.Directory, "."+FailureNoteName+"-"+rand.Text())
	defer func() {
		if result == nil {
			return
		}
		// Also after a failed create: it can fail after the file exists.
		if err := os.Remove(pending); !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}()
	file, err := createDiagnosticNote(pending, r.owner)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(note)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	return replaceDiagnosticNote(pending, filepath.Join(r.config.Directory, FailureNoteName))
}
