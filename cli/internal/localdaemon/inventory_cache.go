package localdaemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/cli/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

// InventoryCacheFile is the last machine list the server answered, under the daemon's state root. A daemon restarted
// while Paperboat is unreachable starts from it, so a restart behaves like a sleep: commands resolve the machines it
// last knew (the peer network itself restores its last verified configuration). The first answered refresh replaces
// it; signing out removes it. Prior art: Tailscale persists its last netmap and loads it before contacting control
// (ipn/ipnlocal/local.go:3246-3277, ipn/ipnlocal/netmapcache/netmapcache.go).
const InventoryCacheFile = "machine-inventory.json"

// inventoryCachePath is the cache file under the daemon's state root; no state root, no cache.
func inventoryCachePath(stateRoot string) string {
	if stateRoot == "" {
		return ""
	}
	return filepath.Join(stateRoot, InventoryCacheFile)
}

// maxInventoryCacheBytes bounds the file read at start.
const maxInventoryCacheBytes = 16 << 20

// restoreCachedMachines seeds a store that has no machines yet with the cached list; the daemon state is unchanged.
func (i *Inventory) restoreCachedMachines() {
	if i.cachePath == "" {
		return
	}
	file, err := os.Open(i.cachePath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			diagnosticlog.TryWarn("machine inventory cache unreadable", "error", err)
		}
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxInventoryCacheBytes+1))
	var machines []localapi.MachineStatus
	if err == nil && len(data) <= maxInventoryCacheBytes {
		err = json.Unmarshal(data, &machines)
	}
	if err != nil || len(machines) == 0 {
		return
	}
	if _, err := i.store.Update(i.clock().UTC(), func(current *localapi.Snapshot) (localapi.Snapshot, error) {
		if current == nil || len(current.Machines) > 0 {
			return localapi.Snapshot{}, errInventoryCacheNotNeeded
		}
		seeded := *current
		seeded.Machines = machines
		return seeded, nil
	}); err != nil && !errors.Is(err, errInventoryCacheNotNeeded) {
		diagnosticlog.TryWarn("machine inventory cache refused", "error", err)
		return
	}
	i.cached = data
}

var errInventoryCacheNotNeeded = errors.New("inventory already has machines")

// saveCachedMachines records an answered machine list; an unchanged list is not rewritten.
func (i *Inventory) saveCachedMachines(machines []localapi.MachineStatus) {
	if i.cachePath == "" {
		return
	}
	data, err := json.Marshal(machines)
	if err != nil || bytes.Equal(data, i.cached) {
		return
	}
	if err := atomicfile.Write(i.cachePath, data, atomicfile.CurrentOwnerOptions(0o600)); err != nil {
		diagnosticlog.TryWarn("machine inventory cache not saved", "error", err)
		return
	}
	i.cached = data
}

// forgetCachedMachines removes the cache when the account signed out or this machine was refused.
func (i *Inventory) forgetCachedMachines() {
	if i.cachePath == "" {
		return
	}
	if err := os.Remove(i.cachePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		diagnosticlog.TryWarn("machine inventory cache not removed", "error", err)
		return
	}
	i.cached = nil
}
