package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// lockArtifactState serializes the TUF store, including failed refreshes that
// have already accepted newer roots. The lock file remains in its protected store.
func lockArtifactState(ctx context.Context, directory string) (func(), error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, ErrArtifactTarget
	}
	if err := secureDirectory(directory); err != nil {
		return nil, err
	}
	f, err := openArtifactLock(filepath.Join(directory, ".metadata.lock"))
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		acquired, err := tryArtifactLock(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		if acquired {
			var once sync.Once
			return func() { once.Do(func() { releaseArtifactLock(f); f.Close() }) }, nil
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func validateArtifactLock(f *os.File, path string) error {
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	named, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(opened, named) {
		return ErrArtifactTarget
	}
	return nil
}
