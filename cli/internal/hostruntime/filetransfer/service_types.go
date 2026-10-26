package filetransfer

import (
	"io"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
)

const (
	MaxFileBytes  = int64(50 << 20)
	MaxBatchBytes = int64(500 << 20)
	MaxBatchFiles = 10
	Retention     = 7 * 24 * time.Hour

	// cancellationDrainTimeout bounds a DELETE while an HTTP upload is
	// unwinding. We must not remove a partial file until its active writer has
	// closed it: Windows rejects removal of an open file.
	cancellationDrainTimeout = 5 * time.Second
)

type Policy struct {
	Revision               string `json:"revision"`
	MaxFileBytes           int64  `json:"max_file_bytes"`
	MaxBatchFiles          int    `json:"max_batch_files"`
	MaxBatchBytes          int64  `json:"max_batch_bytes"`
	MaxConcurrentTransfers int    `json:"max_concurrent_transfers"`
	RetentionSeconds       int64  `json:"retention_seconds"`
	DeliveryTimeoutSeconds int64  `json:"delivery_timeout_seconds"`
	MaxPendingSpoolBytes   int64  `json:"max_pending_spool_bytes"`
	AutomaticMaxBatchBytes int64  `json:"automatic_max_batch_bytes,omitempty"`
}

// AutomaticTransferLimit is the largest Automatic transfer: 2,000,000,000 bytes (PLAN 3b, File transfer modes).
const AutomaticTransferLimit = int64(2_000_000_000)

// Bounds is the per-file and per-batch byte bound: the Automatic batch bound for both when the server sends it,
// otherwise the earlier separate fields (accepted until the server stops sending them).
func (p Policy) Bounds() (file, batch int64) {
	if p.AutomaticMaxBatchBytes > 0 {
		return p.AutomaticMaxBatchBytes, p.AutomaticMaxBatchBytes
	}
	return p.MaxFileBytes, p.MaxBatchBytes
}

func (p Policy) ValidBounds() bool {
	file, batch := p.Bounds()
	return file > 0 && file <= batch && batch <= AutomaticTransferLimit && p.MaxPendingSpoolBytes >= batch
}

var DefaultPolicy = Policy{Revision: "file-transfer-v1", MaxFileBytes: MaxFileBytes, MaxBatchFiles: MaxBatchFiles, MaxBatchBytes: MaxBatchBytes, MaxConcurrentTransfers: 2, RetentionSeconds: int64(Retention / time.Second), DeliveryTimeoutSeconds: 600, MaxPendingSpoolBytes: 1 << 30}

type PolicyStore struct {
	mu     sync.RWMutex
	policy Policy
}

func NewPolicyStore(policy Policy) *PolicyStore {
	if !policy.Valid() {
		policy = DefaultPolicy
	}
	return &PolicyStore{policy: policy}
}

func (s *PolicyStore) Current() Policy { s.mu.RLock(); defer s.mu.RUnlock(); return s.policy }

func (s *PolicyStore) Update(policy Policy) error {
	if !policy.Valid() {
		return &Error{Code: InvalidSize}
	}
	s.mu.Lock()
	s.policy = policy
	s.mu.Unlock()
	return nil
}

func (p Policy) Valid() bool {
	return p.Revision != "" && p.ValidBounds() && p.MaxBatchFiles > 0 && p.MaxBatchFiles <= MaxBatchFiles && p.MaxConcurrentTransfers > 0 && p.MaxConcurrentTransfers <= 2 && p.RetentionSeconds > 0 && p.DeliveryTimeoutSeconds > 0
}

type File struct {
	Basename string `json:"basename"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
}

type CreateRequest struct {
	BatchID              string
	SourceMachineID      string
	DestinationMachineID string
	InitiatingUserID     string
	SessionID            string
	DeliveryClientID     string
	Files                []File
	// MaxBytes and TransferMode are the credential's signed transfer claims (PLAN 3b File transfer modes). When the
	// credential carries them they replace the policy caps as this batch's bound: automatic is bounded by MaxBytes
	// (its declared total), unlimited by nothing but the destination's free space.
	MaxBytes     int64
	TransferMode string
}

type Config struct {
	Root                     string
	PublishRoot              func() (string, error)
	LocalMachineID           string
	Store                    *store.Store
	Now                      func() time.Time
	Random                   io.Reader
	Policy                   *PolicyStore
	CancellationDrainTimeout time.Duration
	// FreeSpace reports the bytes available on the disk holding a path; zero uses the real disk.
	FreeSpace func(string) (uint64, error)
}

type Service struct {
	config            Config
	slotMu            sync.Mutex
	active            int
	updateTransaction string
	updateGeneration  uint64
	operations        int
	operationDone     chan struct{}
	locks             sync.Map
	cancels           sync.Map
	writes            sync.Map
}

type transferCancellation struct {
	once sync.Once
	done chan struct{}
}

type transferWrite struct{ done chan struct{} }
