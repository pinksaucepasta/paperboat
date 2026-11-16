//go:build !linux && !darwin && !windows

package installsource

import "time"

// fileIdentity is unused where no inode/ctime identity is available: every Verify hashes.
type fileIdentity struct{}

func (fileIdentity) changed() time.Time { return time.Time{} }

func fileIdentityOf(string) (fileIdentity, bool) { return fileIdentity{}, false }
