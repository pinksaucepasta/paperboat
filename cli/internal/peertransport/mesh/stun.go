package mesh

import (
	"errors"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/tailcfg"
)

const MaxSTUNServers = 8
const stunRegionBase tailcfg.DERPRegionID = 65536

// ValidateSTUNServers bounds operator-authorized discovery, independently of relay admission.
func ValidateSTUNServers(servers []string) error {
	if len(servers) > MaxSTUNServers {
		return errors.New("too many STUN servers")
	}
	seen := map[string]bool{}
	for _, server := range servers {
		host, port, err := net.SplitHostPort(server)
		n, e := strconv.ParseUint(port, 10, 16)
		if err != nil || e != nil || n == 0 || strconv.FormatUint(n, 10) != port || host == "" || len(host) > 253 || strings.ToLower(host) != host || seen[server] {
			return errors.New("invalid STUN server")
		}
		seen[server] = true
		if ip, e := netip.ParseAddr(host); e == nil {
			if ip.Zone() != "" {
				return errors.New("STUN address must not have a zone")
			}
			ip = ip.Unmap()
			if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				return errors.New("STUN address must be public")
			}
		} else {
			if !strings.Contains(host, ".") {
				return errors.New("STUN hostname must be qualified")
			}
			for _, label := range strings.Split(host, ".") {
				if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
					return errors.New("invalid STUN hostname")
				}
				for _, c := range label {
					if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
						return errors.New("invalid STUN hostname")
					}
				}
			}
		}
	}
	return nil
}

// withSTUNServers never changes a relay region or gives a discovery server a carrier.
func withSTUNServers(dm *tailcfg.DERPMap, servers []string) *tailcfg.DERPMap {
	out := dm.Clone()
	if out.Regions == nil {
		out.Regions = map[tailcfg.DERPRegionID]*tailcfg.DERPRegion{}
	}
	for id := range out.Regions {
		if id >= stunRegionBase && id < stunRegionBase+MaxSTUNServers {
			delete(out.Regions, id)
		}
	}
	for i, server := range servers {
		host, port, _ := net.SplitHostPort(server)
		p, _ := strconv.Atoi(port)
		id := stunRegionBase + tailcfg.DERPRegionID(i)
		node := &tailcfg.DERPNode{Name: "stun-" + strconv.Itoa(i), RegionID: id, HostName: host, STUNPort: p, STUNOnly: true}
		if ip, e := netip.ParseAddr(host); e == nil {
			if ip.Is4() {
				node.IPv4 = host
				node.IPv6 = "none"
			} else {
				node.IPv6 = host
				node.IPv4 = "none"
			}
		}
		out.Regions[id] = &tailcfg.DERPRegion{RegionID: id, RegionCode: node.Name, Nodes: []*tailcfg.DERPNode{node}}
	}
	return out
}

// SetSTUNServers replaces signed discovery configuration without changing relay selection.
func (s *Server) SetSTUNServers(servers []string) error {
	if err := ValidateSTUNServers(servers); err != nil {
		return err
	}
	if s.lb == nil {
		s.STUNServers = slices.Clone(servers)
		return nil
	}
	b := s.lb
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	b.mu.Lock()
	if slices.Equal(b.stunServers, servers) {
		b.mu.Unlock()
		return nil
	}
	b.stunServers = slices.Clone(servers)
	b.dm = withSTUNServers(b.dm, servers)
	dm := b.dm
	b.mu.Unlock()
	b.sys.MagicSock.Get().SetDERPMap(dm)
	return nil
}
