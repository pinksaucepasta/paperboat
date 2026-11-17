//go:build linux || darwin

package privatepreviewproxy

import "github.com/pinksaucepasta/paperboat/cli/internal/machineguard"

func init() { platformPeerOwner = machineguard.ConnectionOwnerUID }

func platformOwnSID() string { return "" }

func peerRefusalReason(err error) string { return machineguard.AttributionRefusalReason(err) }
