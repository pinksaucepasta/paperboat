package enrollment

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

// identityCacheTTL bounds how long a loaded runtime identity is reused. Each authenticated request signs a proof
// and takes a token, and each load stats, chmods and checks the ACL of both identity files (on Windows an access
// check against the process token): a sweep of 14 requests paid that 28 times. A file replaced, resized or
// re-permissioned is seen at once (stat signature); an ACL change only at the TTL (PR-03).
const identityCacheTTL = 10 * time.Second

var identityCacheNow = time.Now

var identityCache struct {
	sync.Mutex
	root            string
	runtime, secret os.FileInfo
	loaded          time.Time
	value           RuntimeIdentity
}

func sameIdentityFile(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.ModTime().Equal(b.ModTime()) && a.Size() == b.Size() && a.Mode() == b.Mode()
}

// cachedRuntimeIdentity returns the identity loaded for root when both files are unchanged since.
func cachedRuntimeIdentity(root string, runtime os.FileInfo) (RuntimeIdentity, bool) {
	identityCache.Lock()
	defer identityCache.Unlock()
	if identityCache.root != root || identityCacheNow().Sub(identityCache.loaded) >= identityCacheTTL || !sameIdentityFile(identityCache.runtime, runtime) {
		return RuntimeIdentity{}, false
	}
	secret, err := os.Lstat(filepath.Join(root, "machine-identity.json"))
	if err != nil || !sameIdentityFile(identityCache.secret, secret) {
		return RuntimeIdentity{}, false
	}
	return identityCache.value, true
}

func storeRuntimeIdentity(root string, runtime os.FileInfo, value RuntimeIdentity) {
	secret, err := os.Lstat(filepath.Join(root, "machine-identity.json"))
	if err != nil {
		return
	}
	identityCache.Lock()
	defer identityCache.Unlock()
	identityCache.root, identityCache.runtime, identityCache.secret = root, runtime, secret
	identityCache.loaded, identityCache.value = identityCacheNow(), value
}
