//go:build !linux && !darwin && !windows

package installsource

// fileIdentity is unused where no inode/ctime identity is available: every Verify hashes.
type fileIdentity struct{}

func fileIdentityOf(string) (fileIdentity, bool) { return fileIdentity{}, false }
