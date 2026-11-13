package edgehttp

// lookupHostLocked finds the route serving host: the one attached under that name, else the one the
// server admitted the host on (hostRoute), the account's own domain on a self-hosted edge.
func (r *DataCarrierPreviewRegistry) lookupHostLocked(host string) (*dataCarrierPreviewRouteEntry, bool) {
	if entry := r.byHost[host]; entry != nil {
		return entry, true
	}
	if r.hostRoute == nil {
		return nil, false
	}
	entry := r.byRoute[r.hostRoute(host)]
	return entry, entry != nil
}
