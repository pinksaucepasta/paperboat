package teaminbox

import serverconfig "github.com/pinksaucepasta/paperboat/server/internal/config"

// ManifestLimitError reports only public configured limits, never file metadata.
type ManifestLimitError struct {
	MaxFileBytes  int64 `json:"max_file_bytes"`
	MaxBatchFiles int   `json:"max_batch_files"`
	MaxBatchBytes int64 `json:"max_batch_bytes"`
}

func (*ManifestLimitError) Error() string {
	return "Reduce the number or size of files to the configured transfer limits."
}
func (*ManifestLimitError) Unwrap() error { return ErrInvalid }
func newManifestLimitError(policy serverconfig.FileTransferPolicy) *ManifestLimitError {
	return &ManifestLimitError{MaxFileBytes: policy.MaxFileBytes, MaxBatchFiles: policy.MaxBatchFiles, MaxBatchBytes: policy.MaxBatchBytes}
}
