package protocol

import "strings"

// FileTransferChunkBytes bounds an independently verified upload commit. It
// retains the existing transfer workload's 1 MiB chunk size without a content key.
const FileTransferChunkBytes = 1 << 20

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
