//go:build windows

package privatepreviewproxy

import (
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
	"golang.org/x/sys/windows"
)

func init() { platformPeerSID = machineguard.ConnectionOwnerSID }

// platformOwnSID is the user this process runs as; empty (so every peer is "another user") if it cannot be read.
func platformOwnSID() string {
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return ""
	}
	return user.User.Sid.String()
}

func peerRefusalReason(err error) string { return machineguard.AttributionRefusalReason(err) }
