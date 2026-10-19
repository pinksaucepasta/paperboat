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
	return p.Revision != "" && p.MaxFileBytes > 0 && p.MaxFileBytes <= MaxFileBytes && p.MaxBatchFiles > 0 && p.MaxBatchFiles <= MaxBatchFiles && p.MaxBatchBytes >= p.MaxFileBytes && p.MaxBatchBytes <= MaxBatchBytes && p.MaxConcurrentTransfers > 0 && p.MaxConcurrentTransfers <= 2 && p.RetentionSeconds > 0 && p.DeliveryTimeoutSeconds > 0 && p.MaxPendingSpoolBytes >= p.MaxBatchBytes
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
