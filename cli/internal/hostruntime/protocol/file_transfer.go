package protocol

// FileTransferChunkBytes bounds an independently verified upload commit. It
// retains the existing transfer workload's 1 MiB chunk size without a content key.
const FileTransferChunkBytes = 1 << 20
