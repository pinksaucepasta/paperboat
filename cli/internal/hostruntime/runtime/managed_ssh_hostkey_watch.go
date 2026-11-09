//go:build darwin || linux || windows

package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	sshHostKeyDirectory = "/etc/ssh"
	// sshHostKeyQuiet is how long the key files stay unchanged before the host reads them. ssh-keygen -A and a
	// package install write the keys one after another (RSA takes a while), and a partial set read in between
	// would become a pending set that the complete set then conflicts with on the server.
	sshHostKeyQuiet = time.Second
)

// watchSSHHostKeys is the host key watch of the production reconciler: the sshd host public key files in
// /etc/ssh. Prior art: tailscaled and headscale react to their own state changes instead of polling; the repo's
// config sync uses fsnotify the same way (kqueue on macOS, inotify on Linux).
func watchSSHHostKeys(ctx context.Context) (<-chan struct{}, error) {
	return watchHostKeyFiles(ctx, sshHostKeyDirectory, sshHostKeyQuiet)
}

// watchHostKeyFiles signals once, after quiet without further events, for each burst of changes to the
// ssh_host_*_key.pub files in dir. The channel is closed when the watch ends (the context, or the directory
// going away), which leaves the caller polling. The directory is watched, not the files, because key
// regeneration deletes and creates them.
func watchHostKeyFiles(ctx context.Context, dir string, quiet time.Duration) (<-chan struct{}, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := watcher.Add(dir); err != nil {
		_ = watcher.Close()
		return nil, err
	}
	signals := make(chan struct{}, 1)
	go func() {
		defer close(signals)
		defer watcher.Close()
		timer := time.NewTimer(time.Hour)
		timer.Stop()
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case event, open := <-watcher.Events:
				if !open {
					return
				}
				if event.Name == dir && event.Has(fsnotify.Remove|fsnotify.Rename) {
					return // the watch is gone with the directory
				}
				if isSSHHostPublicKey(event.Name) {
					timer.Reset(quiet)
				}
			case _, open := <-watcher.Errors:
				if !open {
					return
				}
			case <-timer.C:
				select {
				case signals <- struct{}{}:
				default: // a signal is already waiting
				}
			}
		}
	}()
	return signals, nil
}

func isSSHHostPublicKey(path string) bool {
	name := filepath.Base(path)
	return strings.HasPrefix(name, "ssh_host_") && strings.HasSuffix(name, "_key.pub")
}
