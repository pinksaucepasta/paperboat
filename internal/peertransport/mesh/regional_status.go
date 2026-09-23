// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package mesh

import (
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/tailcfg"
)

// RelayTransport reports the concrete transport carrying a server region.
func (s *Server) RelayTransport(id tailcfg.DERPRegionID) string {
	if s.lb == nil {
		return ""
	}
	return s.lb.sys.MagicSock.Get().RelayTransport(id)
}
