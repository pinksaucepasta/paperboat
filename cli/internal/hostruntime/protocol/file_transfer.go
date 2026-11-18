package protocol

import "strings"

// FileTransferChunkBytes bounds one independently verified upload commit (stored-file pulls).
const FileTransferChunkBytes = 1 << 20

// A native upload is one streamed PATCH per segment, with no acknowledgement inside it: the transport's flow
// control paces the sender, so throughput is not bounded by round-trip time. The receiver fsyncs and commits
// every checkpoint (a cut resumes from the last one) and the segment bounds how long an update waits for it.
const (
	FileTransferSegmentBytes    = 64 << 20
	FileTransferCheckpointBytes = 16 << 20
)

// FileTransferPagination describes one bounded page of authorized metadata.
// Payload bytes are never included in history discovery.
type FileTransferPagination struct {
	Limit      int  `json:"limit"`
	Offset     int  `json:"offset"`
	Total      int  `json:"total"`
	NextOffset *int `json:"next_offset"`
}

func ValidFileTransferHistoryFilters(query, state string) bool {
	if len(query) > 256 || strings.ContainsAny(query, "\x00\r\n") {
		return false
	}
	switch state {
	case "", "created", "uploading", "published", "pending", "delivered", "failed", "canceled", "expired":
		return true
	}
	return false
}
