package teaminbox

import serverconfig "github.com/pinksaucepasta/paperboat/server/internal/config"

// ManifestLimitError reports only public configured limits, never file metadata.
type ManifestLimitError struct {
	MaxBatchFiles int `json:"max_batch_files"`
}

func (*ManifestLimitError) Error() string {
	return "Reduce the number of files to the configured transfer limit."
}
func (*ManifestLimitError) Unwrap() error { return ErrInvalid }
func newManifestLimitError(policy serverconfig.FileTransferPolicy) *ManifestLimitError {
	return &ManifestLimitError{MaxBatchFiles: policy.MaxBatchFiles}
}

// MaximumFileBytes (1 PiB) and MaximumFiles are sanity bounds on a manifest, far above any
// transfer mode's bound, so a declared total never overflows.
const (
	MaximumFileBytes int64 = 1 << 50
	MaximumFiles     int64 = 1 << 10
)

// ManifestTotal is a batch's declared total: every file's size summed.
func ManifestTotal(files []File) int64 {
	var total int64
	for _, file := range files {
		total += file.Size
	}
	return total
}
