package hostinstall

import (
	"fmt"
	"strings"
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
	return strings.HasSuffix(lower, marker) || strings.HasPrefix(lower, "paperboatsessionowner"+marker+"-")
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
	for _, directory := range directories {
		leftovers = append(leftovers, directory)
	}
	if pathEntry {
		leftovers = append(leftovers, "machine PATH entry")
	}
	return leftovers
}
