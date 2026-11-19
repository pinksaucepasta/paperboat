package hostinstall

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/service"
)

// The Windows account sweep decides ownership from observed names and paths.
// These rules are pure so every platform can unit-test them; the SCM, process
// and registry reads that feed them live in purge_sweep_windows.go.

// windowsAccountServiceOwned reports whether an SCM service belongs to one
// account instance. Session owners are PaperboatSessionOwner-<instance>-<gen>,
// so they match on the instance delimiter, not only as a suffix.
// PaperboatUpdateActivator is matched by its bare name as the teardown contract
// requires.
func windowsAccountServiceOwned(name, instance string) bool {
	lower := strings.ToLower(name)
	if lower == "paperboatupdateactivator" {
		return true
	}
	instance = strings.ToLower(instance)
	if instance == "" || !strings.HasPrefix(lower, "paperboat") {
		return false
	}
	marker := "-" + instance
	return strings.HasSuffix(lower, marker) || strings.HasPrefix(lower, "paperboatsessionowner"+marker+"-") || strings.HasPrefix(lower, "paperboatcarrier"+marker+"-")
}

// windowsAccountServices filters SCM service names down to the account's own.
func windowsAccountServices(names []string, instance string) []string {
	var owned []string
	for _, name := range names {
		if windowsAccountServiceOwned(name, instance) {
			owned = append(owned, name)
		}
	}
	return owned
}

// windowsImageUnderRoots reports whether an executable image is one of the
// roots or lies below one. Windows paths compare case-insensitively, and a
// sibling that merely shares a prefix (users\<inst>2) does not match.
func windowsImageUnderRoots(image string, roots []string) bool {
	image = strings.ToLower(strings.TrimRight(image, `\`))
	if image == "" {
		return false
	}
	for _, root := range roots {
		root = strings.ToLower(strings.TrimRight(root, `\`))
		if root != "" && (image == root || strings.HasPrefix(image, root+`\`)) {
			return true
		}
	}
	return false
}

type windowsProcessEntry struct{ id, parent uint32 }

// windowsPayloadProcessIDs selects the account's running processes: the ones the sweep ends and the re-scan
// counts. A process is the account's when its image lies under the account's roots and it runs as one of users
// (the account's owner, LocalSystem for its services, the user doing the purge). The image path alone is not
// enough: another user's process is never ended, and neither is one whose image or user cannot be read; the files
// such a process holds are left to the restart. The ancestry of every keep id is never selected: it is the
// invocation that runs this purge (reset can come through several elevation helpers).
func windowsPayloadProcessIDs(entries []windowsProcessEntry, keep []uint32, roots, users []string, image, user func(uint32) (string, bool)) []uint32 {
	parents := make(map[uint32]uint32, len(entries))
	for _, entry := range entries {
		parents[entry.id] = entry.parent
	}
	protected := map[uint32]bool{}
	for _, start := range keep {
		for id := start; id != 0 && !protected[id]; id = parents[id] {
			protected[id] = true
		}
	}
	var processIDs []uint32
	for _, entry := range entries {
		if entry.id == 0 || protected[entry.id] {
			continue
		}
		path, _ := image(entry.id)
		if !windowsImageUnderRoots(path, roots) {
			continue // the user is read only for the few processes that run the account's images
		}
		if sid, _ := user(entry.id); windowsPayloadProcess(path, sid, roots, users) {
			processIDs = append(processIDs, entry.id)
		}
	}
	return processIDs
}

// windowsPayloadProcess is the rule of windowsPayloadProcessIDs for one process; an unreadable image or user is "".
func windowsPayloadProcess(image, sid string, roots, users []string) bool {
	return sid != "" && windowsImageUnderRoots(image, roots) && slices.ContainsFunc(users, func(allowed string) bool { return strings.EqualFold(allowed, sid) })
}

// windowsOtherAccountEntries returns users-directory entries that belong to
// another account. Any such entry keeps the shared machine guard installed.
func windowsOtherAccountEntries(entries []string, instance string) []string {
	var others []string
	for _, entry := range entries {
		if !strings.EqualFold(entry, instance) {
			others = append(others, entry)
		}
	}
	return others
}

// windowsPathListContains reports whether a PATH value contains directory as an
// exact entry, ignoring case and a trailing separator.
func windowsPathListContains(pathValue, directory string) bool {
	want := strings.TrimRight(directory, `\`)
	if want == "" {
		return false
	}
	for _, entry := range strings.Split(pathValue, ";") {
		if strings.EqualFold(strings.TrimRight(strings.TrimSpace(entry), `\`), want) {
			return true
		}
	}
	return false
}

// windowsAccountLeftovers names every account item still present after the
// sweep. Items are named by kind and identifier; no payload path is included.
func windowsAccountLeftovers(services []string, processIDs []uint32, directories []string, pathEntry bool) []string {
	var leftovers []string
	for _, service := range services {
		leftovers = append(leftovers, "service "+service)
	}
	for _, processID := range processIDs {
		leftovers = append(leftovers, fmt.Sprintf("process %d", processID))
	}
	leftovers = append(leftovers, directories...)
	if pathEntry {
		leftovers = append(leftovers, "machine PATH entry")
	}
	return leftovers
}

// purgeServiceDeclarations removes the declared runtime services of one account instance and their declarations.
// A service registered without its declaration (an interrupted uninstall deleted the declarations first, or a
// delete is still pending) cannot be proven ours here; it is left to the account sweep that ends the purge, which
// deletes this account's services by name and re-scans, so the purge fails only for a service the sweep cannot delete.
func purgeServiceDeclarations(ctx context.Context, directory, instance string, remove func(context.Context, string) error) error {
	var result error
	for _, base := range []string{"PaperboatHost", "PaperboatLocalDaemon", "PaperboatUpdated"} {
		path := filepath.Join(directory, base+"-"+instance+".json")
		if err := remove(ctx, path); errors.Is(err, service.ErrUndeclaredService) {
			continue
		} else if err != nil {
			result = errors.Join(result, hostInstallFailure("remove Windows service "+base, err))
		} else {
			result = errors.Join(result, removeInstallFile(path))
		}
	}
	return result
}

// settledLeftovers repeats the re-scan until it finds nothing or timeout passes. A service that was deleted while a
// handle to it was open stays listed, stopped, until the last handle closes; that happens a moment after the
// processes holding it end, so a single scan right after the sweep reported services that were about to vanish
// ("pb uninstall failed", two stopped Paperboat services, 2026-11-19).
func settledLeftovers(ctx context.Context, timeout, delay time.Duration, scan func() ([]string, error)) ([]string, error) {
	deadline := time.Now().Add(timeout)
	for {
		leftovers, err := scan()
		if err != nil || len(leftovers) == 0 || !time.Now().Add(delay).Before(deadline) {
			return leftovers, err
		}
		select {
		case <-ctx.Done():
			return leftovers, ctx.Err()
		case <-time.After(delay):
		}
	}
}
