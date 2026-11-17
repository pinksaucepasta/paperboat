//go:build darwin || linux

package hostruntimecmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
)

// installBinaryOnly is pb install on a machine without systemd: it puts the binary in place and changes nothing
// else, then tells the user how to run it. Root installs to /usr/local/bin (on every PATH), others to ~/.local/bin.
func installBinaryOnly(executable, directory string, stderr io.Writer) (string, error) {
	if directory == "" {
		directory = "/usr/local/bin"
		if os.Geteuid() != 0 {
			account, err := user.Current()
			if err != nil {
				return "", err
			}
			directory = filepath.Join(account.HomeDir, ".local", "bin")
		}
	}
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return "", errors.New("install directory must be an absolute canonical path")
	}
	target := filepath.Join(directory, "pb")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", fmt.Errorf("creating the install directory failed: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(executable); err != nil || resolved != target {
		if err := copyExecutable(executable, target); err != nil {
			return "", fmt.Errorf("installing pb to %s failed: %w", target, err)
		}
	}
	fmt.Fprintf(stderr, "Installed pb at %s.\n%s\n", target, ForegroundHint(target))
	return target, nil
}

// copyExecutable replaces target atomically, so a running pb at target is never half written.
func copyExecutable(source, target string) (resultErr error) {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(target), ".pb-install-*")
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, os.Remove(out.Name()))
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return errors.Join(err, out.Close())
	}
	if err := errors.Join(out.Chmod(0o755), out.Sync(), out.Close()); err != nil {
		return err
	}
	return os.Rename(out.Name(), target)
}
