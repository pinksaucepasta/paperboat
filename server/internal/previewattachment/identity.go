package previewattachment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/canonicaljson"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/nodeauth"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

// PreviewEdgeIdentity is the server-known identity of one registered edge
// process. The opaque process epoch is required in addition to the stable
// node ID so an old overlapping process cannot mutate the replacement's
// admissions.
type PreviewEdgeIdentity struct {
	NodeID       string
	ProcessEpoch string
}

// PreviewEdgeRequestVerifier authenticates the edge control request and
// returns the server-known edge node and process identity. Implementations
// should bind this identity to the edge's mTLS peer certificate where
// available.
type PreviewEdgeRequestVerifier interface {
	VerifyPreviewEdgeRequest(context.Context, *http.Request, []byte) (PreviewEdgeIdentity, error)
}

// DBPreviewEdgeRequestVerifier is the default control-plane verifier. It
// reuses the existing edge-control credential and requires the node ID to be
// a currently registered node. The canonical data-plane peer verifier still
// validates the machine certificate against Binding.MachineIdentityPublicKey
// before installing a carrier route.
type DBPreviewEdgeRequestVerifier struct {
	nodeCredentials nodeauth.Resolver
	db              *db.DB
	credential      string
	now             func() time.Time
}

func NewDBPreviewEdgeRequestVerifier(database *db.DB, credential string) (*DBPreviewEdgeRequestVerifier, error) {
	if database == nil || database.Pool() == nil || strings.TrimSpace(credential) == "" {
		return nil, fmt.Errorf("%w: edge request verifier dependencies are incomplete", ErrInvalid)
	}
	return &DBPreviewEdgeRequestVerifier{db: database, credential: credential, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (v *DBPreviewEdgeRequestVerifier) SetClock(now func() time.Time) error {
	if v == nil || now == nil {
		return fmt.Errorf("%w: nil edge verifier clock", ErrInvalid)
	}
	v.now = now
	return nil
}

func (v *DBPreviewEdgeRequestVerifier) VerifyPreviewEdgeRequest(ctx context.Context, r *http.Request, body []byte) (PreviewEdgeIdentity, error) {
	if v == nil || v.db == nil || v.db.Pool() == nil || r == nil || ctx == nil {
		return PreviewEdgeIdentity{}, ErrUnauthorized
	}
	identity, err := edgeNodeIdentity(r, body)
	if err != nil {
		return PreviewEdgeIdentity{}, ErrUnauthorized
	}
	matched, err := nodeauth.Matches(ctx, v.nodeCredentials, r, v.credential, identity.NodeID)
	if err != nil {
		return PreviewEdgeIdentity{}, err
	}
	if !matched {
		return PreviewEdgeIdentity{}, ErrUnauthorized
	}
	var currentEpoch string
	var available bool
	err = v.db.Pool().QueryRow(ctx, verifyPreviewEdgeNodeSQL, identity.NodeID, v.clock()).Scan(&currentEpoch, &available)
	if errors.Is(err, pgx.ErrNoRows) {
		return PreviewEdgeIdentity{}, ErrUnauthorized
	}
	if err != nil {
		return PreviewEdgeIdentity{}, err
	}
	if !available || currentEpoch != identity.ProcessEpoch {
		return PreviewEdgeIdentity{}, ErrUnauthorized
	}
	identity.ProcessEpoch = currentEpoch
	return identity, nil
}

func (v *DBPreviewEdgeRequestVerifier) clock() time.Time {
	if v == nil || v.now == nil {
		return time.Now().UTC()
	}
	return v.now().UTC()
}

func edgeNodeIdentity(r *http.Request, raw []byte) (PreviewEdgeIdentity, error) {
	var headerID, headerEpoch string
	if values := r.Header.Values("X-Paperboat-Edge-Node-ID"); len(values) > 1 {
		return PreviewEdgeIdentity{}, ErrUnauthorized
	} else if len(values) == 1 {
		headerID = strings.TrimSpace(values[0])
		if headerID != values[0] || !validID(headerID) {
			return PreviewEdgeIdentity{}, ErrUnauthorized
		}
	}
	if values := r.Header.Values("X-Paperboat-Edge-Process-Epoch"); len(values) > 1 {
		return PreviewEdgeIdentity{}, ErrUnauthorized
	} else if len(values) == 1 {
		headerEpoch = strings.TrimSpace(values[0])
		if headerEpoch != values[0] || connectorprotocol.ValidateOpaqueEpoch(headerEpoch) != nil {
			return PreviewEdgeIdentity{}, ErrUnauthorized
		}
	}
	var bodyID string
	var bodyEpoch string
	if len(raw) != 0 {
		if err := canonicaljson.RejectDuplicateFields(raw); err != nil {
			return PreviewEdgeIdentity{}, ErrUnauthorized
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return PreviewEdgeIdentity{}, ErrUnauthorized
		}
		if value, ok := object["edge_node_id"]; ok {
			if err := json.Unmarshal(value, &bodyID); err != nil || !validID(bodyID) {
				return PreviewEdgeIdentity{}, ErrUnauthorized
			}
		}
		if value, ok := object["edge_process_epoch"]; ok {
			if err := json.Unmarshal(value, &bodyEpoch); err != nil || connectorprotocol.ValidateOpaqueEpoch(bodyEpoch) != nil {
				return PreviewEdgeIdentity{}, ErrUnauthorized
			}
		}
	}
	if headerID != "" && bodyID != "" && headerID != bodyID {
		return PreviewEdgeIdentity{}, ErrUnauthorized
	}
	if headerEpoch != "" && bodyEpoch != "" && headerEpoch != bodyEpoch {
		return PreviewEdgeIdentity{}, ErrUnauthorized
	}
	if headerID == "" {
		headerID = bodyID
	}
	if headerEpoch == "" {
		headerEpoch = bodyEpoch
	}
	if !validID(headerID) || connectorprotocol.ValidateOpaqueEpoch(headerEpoch) != nil {
		return PreviewEdgeIdentity{}, ErrUnauthorized
	}
	return PreviewEdgeIdentity{NodeID: headerID, ProcessEpoch: headerEpoch}, nil
}

const verifyPreviewEdgeNodeSQL = `
SELECT node.process_epoch,
       (
         ((node.state = 'ready' AND node.ready) OR node.state = 'draining')
         AND (node.last_heartbeat_at IS NULL OR node.last_heartbeat_at > $2::timestamptz - interval '2 minutes')
         AND (node.drain_deadline IS NULL OR node.drain_deadline > $2::timestamptz)
       ) AS available
FROM control_tunnel_nodes AS node
WHERE node.id = $1`

func (v *DBPreviewEdgeRequestVerifier) SetNodeCredentials(resolver nodeauth.Resolver) {
	v.nodeCredentials = resolver
}

var (
	ErrInvalid      = errors.New("invalid machine or edge identity")
	ErrUnauthorized = errors.New("machine proof does not authorize request")
)

// MachineProof is the result of VerifyMachineRequest: the exact machine and account a signed request
// proves. A caller-supplied account or machine is never authority.
type MachineProof struct {
	UserID                 string
	MachineID              string
	OperationID            string
	InstallationGeneration uint64
}

func (p MachineProof) Validate() error {
	if !validID(p.UserID) || !validID(p.MachineID) || !validID(p.OperationID) {
		return fmt.Errorf("%w: incomplete machine proof", ErrInvalid)
	}
	if p.InstallationGeneration == 0 {
		return fmt.Errorf("%w: installation generation must be positive", ErrInvalid)
	}
	return nil
}

// MachineProofVerifier must verify Authorization, X-Paperboat-Machine-Identity, X-Paperboat-Machine-Proof,
// method, path, and the exact raw body before returning the proof fields.
type MachineProofVerifier interface {
	VerifyMachineRequest(context.Context, *http.Request, []byte) (MachineProof, error)
}

func validID(value string) bool {
	if len(value) < 3 || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}
