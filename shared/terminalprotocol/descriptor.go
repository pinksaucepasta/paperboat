package terminalprotocol

import "time"

// The connection descriptor: what the control plane hands a CLI that wants to reach a machine (terminal, file transfer),
// and the per-operation descriptors for exec, SSH and config compare. The server's output defines the documents; the CLI
// decodes them tolerantly and validates the parts it uses.

// DescriptorSchemaV1 is the schema of a connection descriptor.
const DescriptorSchemaV1 = "paperboat.environment-connection/v1"

const (
	EnvironmentMachine = "machine"

	CapabilityTerminal     = "terminal"
	CapabilityFileTransfer = "file_transfer"
	CapabilityPreview      = "preview"
)

// Descriptor is the connection descriptor of one machine.
type Descriptor struct {
	Schema string `json:"schema"`
	Issuer string `json:"issuer"`
	// MachineGeneration is the machine's installation generation; the CLI binds a shared-session join to it and
	// refuses a descriptor that does not carry one.
	MachineGeneration int64         `json:"machine_generation,omitempty"`
	Connectable       bool          `json:"connectable"`
	ExpiresAt         time.Time     `json:"expires_at"`
	Environment       Environment   `json:"environment"`
	Capabilities      []string      `json:"capabilities,omitempty"`
	Terminal          *Terminal     `json:"terminal,omitempty"`
	FileTransfer      *FileTransfer `json:"file_transfer,omitempty"`
	Status            string        `json:"status"`
	Reason            string        `json:"reason"`
	RetryAfterSeconds int           `json:"retry_after_seconds"`
}

// Environment names the machine a descriptor is about. ID is the environment identity, ResourceID the machine's.
type Environment struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	ResourceID string `json:"resource_id"`
	Alias      string `json:"alias"`
	State      string `json:"state"`
	Root       string `json:"root"`
}

// Auth is the credential a descriptor carries for one capability.
type Auth struct {
	Method          string    `json:"method"`
	Ticket          string    `json:"ticket,omitempty"`
	Token           string    `json:"token,omitempty"`
	ExpiresAt       time.Time `json:"expires_at"`
	Scopes          []string  `json:"scopes"`
	AccessSessionID string    `json:"access_session_id,omitempty"`
}

// Terminal is how to attach a terminal session.
type Terminal struct {
	Protocol  string            `json:"protocol"`
	Endpoints TerminalEndpoints `json:"endpoints"`
	Auth      Auth              `json:"auth"`
	SessionID string            `json:"session_id"`
	CWD       string            `json:"cwd"`
}

// TerminalEndpoints are the edge endpoints of a machine; a customer-edge machine has only wss, and an exec descriptor
// for a client that runs over its native peer connection alone has none.
type TerminalEndpoints struct {
	QUIC string `json:"quic,omitempty"`
	WSS  string `json:"wss,omitempty"`
}

// FileTransferPolicy is the file-transfer bounds the server signs for a machine pair.
type FileTransferPolicy struct {
	Revision               string `json:"revision"`
	MaxBatchFiles          int    `json:"max_batch_files"`
	AutomaticMaxBatchBytes int64  `json:"automatic_max_batch_bytes"`
	MaxConcurrentTransfers int    `json:"max_concurrent_transfers"`
	RetentionSeconds       int64  `json:"retention_seconds"`
	DeliveryTimeoutSeconds int64  `json:"delivery_timeout_seconds"`
	MaxPendingSpoolBytes   int64  `json:"max_pending_spool_bytes"`
}

// AutomaticTransferLimit is the largest Automatic transfer: 2,000,000,000 bytes (PLAN 3b, File transfer modes).
const AutomaticTransferLimit = int64(2_000_000_000)

// Bounds is the per-file and per-batch byte bound: the server publishes one Automatic batch bound, which limits every
// file as well as the batch.
func (p FileTransferPolicy) Bounds() (file, batch int64) {
	return p.AutomaticMaxBatchBytes, p.AutomaticMaxBatchBytes
}

// ValidBounds reports whether the policy's bounds are usable.
func (p FileTransferPolicy) ValidBounds() bool {
	file, batch := p.Bounds()
	return file > 0 && file <= batch && batch <= AutomaticTransferLimit && p.MaxPendingSpoolBytes >= batch
}

// FileTransfer is how to run file transfers between two machines.
type FileTransfer struct {
	Endpoint             string             `json:"endpoint"`
	SourceMachineID      string             `json:"source_machine_id"`
	DestinationMachineID string             `json:"destination_machine_id"`
	InitiatingUserID     string             `json:"initiating_user_id"`
	Auth                 Auth               `json:"auth"`
	Policy               FileTransferPolicy `json:"policy"`
}

// Operation is a per-operation descriptor (exec, managed SSH): one credential for one operation on one machine.
type Operation struct {
	OperationID string            `json:"operation_id"`
	Environment Environment       `json:"environment"`
	Endpoints   TerminalEndpoints `json:"endpoints"`
	Auth        Auth              `json:"auth"`
	ExpiresAt   time.Time         `json:"expires_at"`
}
