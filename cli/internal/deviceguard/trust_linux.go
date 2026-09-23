//go:build linux

package deviceguard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
)

var trustedLinuxRoots sync.Map

// Only public, suffix-constrained roots reach the system trust store. The signing
// key remains in the guard's root-only state directory.
func installCATrust(ctx context.Context, owner, suffix string, certificatePEM []byte) error {
	rootDigest := sha256.Sum256(certificatePEM)
	if _, ok := trustedLinuxRoots.Load(rootDigest); ok {
		return nil
	}
	directory := "/usr/local/share/ca-certificates"
	if err := protectedDirectory(directory, 0755); err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(suffix))
	path := filepath.Join(directory, fmt.Sprintf("paperboat-deviceguard-%x.crt", digest[:12]))
	if info, err := os.Lstat(path); err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("unsafe device guard trust certificate")
		}
		previous, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(previous, certificatePEM) {
			return fmt.Errorf("device guard trust certificate conflicts with installed root")
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		file, err := os.CreateTemp(directory, ".paperboat-ca-")
		if err != nil {
			return err
		}
		temporary := file.Name()
		defer os.Remove(temporary)
		if _, err = file.Write(certificatePEM); err == nil {
			err = file.Chmod(0644)
		}
		if err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err = os.Rename(temporary, path); err != nil {
			return err
		}
	}
	// Re-run after an interrupted installation too: the source file existing does
	// not establish that the trust database update completed.
	if output, err := exec.CommandContext(ctx, "update-ca-certificates").CombinedOutput(); err != nil {
		return fmt.Errorf("install private-name certificate trust: %w: %s", err, output)
	}
	trustedLinuxRoots.Store(rootDigest, struct{}{})
	return nil
}
