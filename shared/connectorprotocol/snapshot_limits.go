package connectorprotocol

// MaximumSnapshotBytes bounds the size of one server snapshot or list answer an edge reads or the server builds
// (ingress decisions, runtime carrier admissions, private access admissions). It is a memory bound on one response,
// well above any node's real snapshot (about 1 KB per entry, so hundreds of thousands of entries); no count of entries is limited.
const MaximumSnapshotBytes = 256 << 20

// VaultRecordSnapshotOverhead covers per-record metadata in addition to the encoded envelope,
// writer key and document ID. The producer and both snapshot consumers charge the same amount.
const VaultRecordSnapshotOverhead = 512

const MaximumVaultRecordsPageSize = 200

// MaximumVaultRecordsPageWireBytes allows 200 maximum-size base64url envelopes
// (about 47 KiB each), plus JSON metadata and the cursor.
const MaximumVaultRecordsPageWireBytes = 16 << 20
