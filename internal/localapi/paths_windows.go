//go:build windows

package localapi

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// Paths is the per-user local API layout. Windows uses a named pipe for the
// API endpoint and LocalAppData Known Folder for state. This avoids trusting a
// caller-controlled environment variable for the local security boundary.
type Paths struct {
	StateRoot   string
	RuntimeRoot string
	SocketPath  string
	LockPath    string
}

func CurrentPaths(uid int) (Paths, error) {
	if uid < 0 {
		return Paths{}, ErrInvalidConfig
	}
	base, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return Paths{}, ErrInvalidConfig
	}
	if !filepath.IsAbs(base) || filepath.Clean(base) != base {
		return Paths{}, ErrInvalidConfig
	}
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || tokenUser == nil || tokenUser.User.Sid == nil || !tokenUser.User.Sid.IsValid() {
		return Paths{}, ErrInvalidConfig
	}
	ownerSID := tokenUser.User.Sid.String()
	if ownerSID == "" {
		return Paths{}, ErrInvalidConfig
	}
	stateRoot := filepath.Join(filepath.Clean(base), "Paperboat", "state")
	return WindowsPaths(stateRoot, ownerSID)
}

// WindowsPaths resolves the local endpoint for an already authenticated OS
// user. LocalSystem callers use it to address the enrolled user's daemon
// without accidentally selecting LocalSystem's own pipe.
func WindowsPaths(stateRoot, ownerSID string) (Paths, error) {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return Paths{}, ErrInvalidConfig
	}
	sid, err := windows.StringToSid(ownerSID)
	if err != nil || sid == nil || !sid.IsValid() || sid.String() != ownerSID {
		return Paths{}, ErrInvalidConfig
	}
	digest := sha256.Sum256([]byte(ownerSID))
	pipeSuffix := hex.EncodeToString(digest[:12])
	runtimeRoot := filepath.Join(stateRoot, "run")
	return Paths{
		StateRoot:   stateRoot,
		RuntimeRoot: runtimeRoot,
		SocketPath:  `\\.\pipe\paperboat-local-api-` + pipeSuffix,
		LockPath:    filepath.Join(stateRoot, "daemon.lock"),
	}, nil
}
