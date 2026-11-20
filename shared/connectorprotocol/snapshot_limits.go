package connectorprotocol

// MaximumSnapshotBytes bounds the size of one server snapshot or list answer an edge reads or the server builds
// (ingress decisions, runtime carrier admissions, private access admissions). It is a memory bound on one response,
// well above any node's real snapshot (about 1 KB per entry, so hundreds of thousands of entries); no count of entries is limited.
const MaximumSnapshotBytes = 256 << 20
