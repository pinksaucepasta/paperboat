package daemonrpc

import "slices"

// PeerUpdate is one machine of the signed-in account as the daemon's local access sees it: its name, its
// local address and the ports it exports. It replaced the generated message of the deleted gRPC control sync
// (F21-07); the getters are nil-safe like the generated ones.
type PeerUpdate struct {
	PeerId        string
	Alias         string
	AssignedIp    string
	Online        bool
	Approved      bool
	ExportedPorts []int32
}

func (p *PeerUpdate) GetPeerId() string {
	if p == nil {
		return ""
	}
	return p.PeerId
}

func (p *PeerUpdate) GetAlias() string {
	if p == nil {
		return ""
	}
	return p.Alias
}

func (p *PeerUpdate) GetAssignedIp() string {
	if p == nil {
		return ""
	}
	return p.AssignedIp
}

func (p *PeerUpdate) GetOnline() bool { return p != nil && p.Online }

func (p *PeerUpdate) GetApproved() bool { return p != nil && p.Approved }

func (p *PeerUpdate) GetExportedPorts() []int32 {
	if p == nil {
		return nil
	}
	return p.ExportedPorts
}

// Clone is an independent copy.
func (p *PeerUpdate) Clone() *PeerUpdate {
	if p == nil {
		return nil
	}
	copied := *p
	copied.ExportedPorts = slices.Clone(p.ExportedPorts)
	return &copied
}
