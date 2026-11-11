// Package recording is the wire contract of Teams session recordings between the server, the
// recording machine (host), the CLI and the console. Recordings are made and kept on the machine;
// the server keeps an index, the team recording key's public half and each permitted viewer's
// sealed copy of its private half, and issues replay tickets after writing the replay audit event.
// The segment format is not here: the server never reads recording bytes (cli/internal/recording).
package recording

import (
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"regexp"
	"slices"
	"time"
)

// Topic is the machine-event topic that moves when a machine's Policy may have changed.
const Topic = "recording_policy"

// PolicyPath is the machine-credential endpoint that returns this machine's Policy.
const PolicyPath = "/v1/machine/recording-policy"

// Who may start a recording by hand (team setting).
const (
	ManualStartAnyone = "anyone" // anyone attached to the terminal session
	ManualStartAdmins = "admins" // only Policy.AdminAccounts
)

// Why a recording started.
const (
	TriggerRule   = "rule"
	TriggerManual = "manual"
)

// Why a recording stopped or was deleted on the machine.
const (
	ReasonStopped      = "stopped"
	ReasonSessionEnded = "session_ended"
	ReasonQuota        = "quota"
	ReasonPolicy       = "policy"
	ReasonError        = "error"
	ReasonRetention    = "retention"
)

// Audit backlog event kinds (usermachines OfflineAuditEvent.Kind) that carry a Report.
const (
	KindStarted = "recording_started"
	KindStopped = "recording_stopped"
	KindDeleted = "recording_deleted"
)

// Index states of a recording.
const (
	StateRecording = "recording"
	StateEnded     = "ended"
	StateDeleted   = "deleted" // removed from the machine; the row stays until server retention
)

// Replay ticket claims (a machine JWT checked by the host's verifier).
const (
	CredentialClass = "recording_replay"
	Scope           = "recording:read"
	ResourceKind    = "recording"
	// TicketLifetime bounds exp - iat; the host keeps the jti in its replay cache that long.
	TicketLifetime = 5 * time.Minute
)

// Bounds shared by producer and consumer.
const (
	MaxAccounts = 64
	// SealedBytes is the size of a sealed key bundle (cli/internal/recording bundle.go): HPKE base mode, DHKEM
	// X25519 + HKDF-SHA256 + AES-256-GCM, enc (32) || ciphertext of X25519 private (32) || Ed25519 private (64)
	// plus the tag (16); info "paperboat recording key bundle v1", aad team_id "\x00" decimal generation of the
	// sealed bundle.
	SealedBytes = 32 + 32 + 64 + 16
	// MaxReportAge is how old a Report may be when it reaches the server (the audit backlog bound).
	MaxReportAge = 7 * 24 * time.Hour
)

// Key is the public half of one generation of the team recording key. Hosts seal each
// recording's data key to PublicKey (X25519). They pin generation 1 on first use and accept
// generation N+1 only when RotationSignature verifies under the pinned generation N
// SigningPublicKey (VerifyRotation).
type Key struct {
	Generation        int64  `json:"generation"`
	PublicKey         []byte `json:"public_key"`                   // X25519, 32 bytes
	SigningPublicKey  []byte `json:"signing_public_key"`           // Ed25519, 32 bytes
	RotationSignature []byte `json:"rotation_signature,omitempty"` // absent at generation 1
}

// Policy is what one machine needs to decide recordings on its own, online and offline.
type Policy struct {
	Version int64 `json:"version"`
	// Enabled is false without the Teams entitlement or on a machine without an owner team.
	Enabled bool   `json:"enabled"`
	TeamID  string `json:"team_id,omitempty"`
	// Keys are every generation of the team recording key, oldest first; the last is current. A host that
	// pinned generation g verifies g+1..last in order (VerifyRotation), so it catches up after missing
	// rotations while offline. Empty until an admin sets the key up.
	Keys           []Key    `json:"keys"`
	RecordAccounts []string `json:"record_accounts"`
	ManualStart    string   `json:"manual_start"`
	AdminAccounts  []string `json:"admin_accounts"` // owner and admins: stop rule recordings, start when admins-only
	RetentionDays  int      `json:"retention_days"`
	DiskQuotaMB    int      `json:"disk_quota_mb"`
}

// ValidManualStart reports whether v is a manual-start setting.
func ValidManualStart(v string) bool { return v == ManualStartAnyone || v == ManualStartAdmins }

// ValidSealed bounds a sealed key blob (a viewer's copy or a rotation's previous key).
func ValidSealed(b []byte) bool { return len(b) == SealedBytes }

// Valid checks a policy a machine received before it acts on it: the settings, the bounds, and a key chain of
// valid, consecutive generations from 1 (each rotation's signature is checked against the pinned key with
// VerifyRotation).
func (p Policy) Valid() bool {
	if !ValidManualStart(p.ManualStart) || p.RetentionDays < 1 || p.DiskQuotaMB < 1 || len(p.RecordAccounts) > maxPolicyAccounts || len(p.AdminAccounts) > maxPolicyAccounts {
		return false
	}
	for i, k := range p.Keys {
		if !k.ValidKey() || k.Generation != int64(i)+1 {
			return false
		}
	}
	return !p.Enabled || p.TeamID != ""
}

// maxPolicyAccounts bounds a policy's account lists (team size is bounded well below it).
const maxPolicyAccounts = 100_000

// Report is the recording part of an audit backlog event (kinds KindStarted, KindStopped,
// KindDeleted).
type Report struct {
	RecordingID    string    `json:"recording_id"`
	SessionID      string    `json:"session_id,omitempty"`       // started
	Trigger        string    `json:"trigger,omitempty"`          // started
	ActorAccountID string    `json:"actor_account_id,omitempty"` // manual start or stop
	KeyGeneration  int64     `json:"key_generation,omitempty"`   // started
	Accounts       []string  `json:"accounts,omitempty"`         // started: attached; stopped: everyone attached while recording
	Reason         string    `json:"reason,omitempty"`           // stopped, deleted
	Bytes          int64     `json:"bytes,omitempty"`            // stopped
	Segments       int64     `json:"segments,omitempty"`         // stopped
	StartedAt      time.Time `json:"started_at,omitzero"`        // started
	EndedAt        time.Time `json:"ended_at,omitzero"`          // stopped
}

var (
	idPattern          = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
	recordingIDPattern = regexp.MustCompile(`^rec_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// ValidRecordingID reports whether id has the host's rec_<uuid> form.
func ValidRecordingID(id string) bool { return recordingIDPattern.MatchString(id) }

// ErrInvalidReport is returned for a Report that does not fit its kind.
var ErrInvalidReport = errors.New("recording report is invalid")

// Validate checks r for kind; now bounds the times.
func (r Report) Validate(kind string, now time.Time) error {
	if !ValidRecordingID(r.RecordingID) || len(r.Accounts) > MaxAccounts || r.Bytes < 0 || r.Segments < 0 {
		return ErrInvalidReport
	}
	for _, a := range r.Accounts {
		if !idPattern.MatchString(a) {
			return ErrInvalidReport
		}
	}
	if r.ActorAccountID != "" && !idPattern.MatchString(r.ActorAccountID) {
		return ErrInvalidReport
	}
	inWindow := func(t time.Time) bool { return !t.Before(now.Add(-MaxReportAge)) && !t.After(now.Add(5*time.Minute)) }
	switch kind {
	case KindStarted:
		manual := r.Trigger == TriggerManual
		if !idPattern.MatchString(r.SessionID) || (r.Trigger != TriggerRule && !manual) || manual != (r.ActorAccountID != "") ||
			r.KeyGeneration < 1 || !inWindow(r.StartedAt) || !r.EndedAt.IsZero() || r.Reason != "" {
			return ErrInvalidReport
		}
	case KindStopped:
		if !slices.Contains([]string{ReasonStopped, ReasonSessionEnded, ReasonQuota, ReasonPolicy, ReasonError}, r.Reason) ||
			(r.Reason == ReasonStopped) != (r.ActorAccountID != "") || !inWindow(r.EndedAt) {
			return ErrInvalidReport
		}
	case KindDeleted:
		if r.Reason != ReasonRetention && r.Reason != ReasonQuota {
			return ErrInvalidReport
		}
	default:
		return ErrInvalidReport
	}
	return nil
}

// Recording is one index row as listed to accounts.
type Recording struct {
	ID          string     `json:"id"`
	MachineID   string     `json:"machine_id"`
	MachineName string     `json:"machine_name"`
	SessionID   string     `json:"session_id"`
	Trigger     string     `json:"trigger"`
	StartedBy   string     `json:"started_by,omitempty"` // account, manual recordings only
	Accounts    []string   `json:"accounts"`
	StartedAt   time.Time  `json:"started_at"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	Bytes       int64      `json:"bytes"`
	Segments    int64      `json:"segments"`
	State       string     `json:"state"`
}

// List is GET /v1/teams/{team}/recordings?machine=&session=&limit=&offset=.
type List struct {
	Recordings []Recording `json:"recordings"`
	Total      int64       `json:"total"`
}

// SealedKey is a viewer's copy of one generation's private bundle (X25519 private key and Ed25519
// signing private key), HPKE-sealed by a client to the viewer's vault sharing key. Opaque to the
// server.
type SealedKey struct {
	Generation int64  `json:"generation"`
	Sealed     []byte `json:"sealed"`
}

// ChainLink opens older recordings after a rotation: SealedPrevious is the generation-1 private
// bundle sealed to Generation's public key.
type ChainLink struct {
	Generation     int64  `json:"generation"`
	SealedPrevious []byte `json:"sealed_previous"`
}

// ReplayRequest is POST /v1/recordings/{id}/replay with exactly one of the two fields. The CLI names
// its client session: the peer network then admits it to the machine for the consumer NativeConsumer
// until the ticket expires, and the ticket is bound to it. A browser sends its E2E public key hash
// and gets a browser ticket (role "recording") and a stream URL instead of a machine ticket.
type ReplayRequest struct {
	CLIClientSessionID     string `json:"cli_client_session_id,omitempty"`
	BrowserPublicKeySHA256 string `json:"browser_public_key_sha256,omitempty"`
}

// NativeConsumer is the peer stream consumer and network capability of a CLI replay.
const NativeConsumer = "recording"

// Replay answers ReplayRequest. The server wrote the replay audit event before answering.
type Replay struct {
	Ticket        string      `json:"ticket"`
	StreamURL     string      `json:"stream_url,omitempty"` // browser form only
	MachineID     string      `json:"machine_id"`
	TeamID        string      `json:"team_id"`        // the aad of the viewer's copy and the chain
	KeyGeneration int64       `json:"key_generation"` // the recording's
	ViewerKey     SealedKey   `json:"viewer_key"`
	Chain         []ChainLink `json:"chain"` // from ViewerKey.Generation down to KeyGeneration+1
	// Browser form only, as every browser ticket answer: the WebSocket subprotocols (the E2E protocol and
	// "pb-ticket.<ticket>"), the ticket's expiry, role "recording", and the machine owner's transport root the
	// browser pins before any byte moves (root_public_key base64url without padding, root_fingerprint hex).
	Subprotocols    []string  `json:"subprotocols,omitempty"`
	ExpiresAt       time.Time `json:"expires_at,omitzero"`
	Role            string    `json:"role,omitempty"`
	EdgeNodeID      string    `json:"edge_node_id,omitempty"`
	OwnerAccountID  string    `json:"owner_account_id,omitempty"`
	RootKeyID       string    `json:"root_key_id,omitempty"`
	RootPublicKey   string    `json:"root_public_key,omitempty"`
	RootFingerprint string    `json:"root_fingerprint,omitempty"`
}

// Viewer is one account with replay permission somewhere in the team.
type Viewer struct {
	AccountID        string `json:"account_id"`
	SharingPublicKey []byte `json:"sharing_public_key,omitempty"` // empty: no vault yet, cannot be sealed for
	HasGrant         bool   `json:"has_grant"`                    // holds a copy of the current generation
}

// KeyState is GET /v1/teams/{team}/recording-key.
type KeyState struct {
	Key         *Key     `json:"key"`
	ManualStart string   `json:"manual_start"`
	Viewers     []Viewer `json:"viewers"`
	// MyKey is the caller's own copy of the current generation, so a holder can seal it for viewers added later;
	// absent when the caller has none or it is not sealed to their current vault.
	MyKey *SealedKey `json:"my_key,omitempty"`
}

// Grant is one sealed copy for one viewer, bound to the sharing key it was sealed to.
type Grant struct {
	AccountID        string `json:"account_id"`
	SharingPublicKey []byte `json:"sharing_public_key"`
	Sealed           []byte `json:"sealed"`
}

// KeyCreate is POST /v1/teams/{team}/recording-key: generation 1, or a rotation signed by the
// current generation's signing key with the current private bundle sealed to the new key.
type KeyCreate struct {
	Key            Key     `json:"key"`
	SealedPrevious []byte  `json:"sealed_previous,omitempty"`
	Grants         []Grant `json:"grants"`
}

// GrantsPut is PUT /v1/teams/{team}/recording-key/grants.
type GrantsPut struct {
	Generation int64   `json:"generation"`
	Grants     []Grant `json:"grants"`
}

// Settings is PUT /v1/teams/{team}/recording-settings.
type Settings struct {
	ManualStart string `json:"manual_start"`
}

// Live recording state and control on a terminal attachment (terminal.v1, browser E2E or native). The actor of a
// control request is the attachment's account; the host decides with Policy (start: Enabled, a key, and anyone or
// an admin per ManualStart; stop: a manual recording by its starter or an admin, a rule recording only by an admin).
const (
	ActionStart    = "record-start" // request {"action":ActionStart,"session_id"}; answers {"recording":State}
	ActionStop     = "record-stop"  // request {"action":ActionStop,"session_id"}; answers {}
	EventRecording = "recording"    // stream event {"event":EventRecording,"session_id","recording":State|null}
)

// Replay stream on the machine (browser E2E or native consumer NativeConsumer): request {"action":ReadAction,
// "recording_id"} on Capability, then binary frames each carrying one segment file behind a 4-byte big-endian
// length, then the event {"event":EventStreamEnd,"segments":n}.
const (
	Capability     = "recording.v1"
	ReadAction     = "read"
	EventStreamEnd = "recording_stream_end"
)

// Control refusals (terminal error codes); not_found_or_forbidden is the terminal's own.
const (
	ErrorUnavailable = "recording_unavailable" // not enabled, no key, or the recorder cannot start
	ErrorActive      = "recording_active"      // start while already recording
	ErrorInactive    = "recording_inactive"    // stop while not recording
)

// State is a session's live recording: in the attach snapshot (Snapshot.recording) before any output frame, absent
// when not recording, and in every EventRecording event (null when it stopped). Every attached client shows REC
// while it is present.
type State struct {
	ID        string    `json:"id"`
	Trigger   string    `json:"trigger"`
	StartedBy string    `json:"started_by,omitempty"` // manual recordings
	StartedAt time.Time `json:"started_at"`
}

// Valid checks a State a client received.
func (s State) Valid() bool {
	return ValidRecordingID(s.ID) && !s.StartedAt.IsZero() &&
		(s.Trigger == TriggerRule && s.StartedBy == "" || s.Trigger == TriggerManual && idPattern.MatchString(s.StartedBy))
}

// Typed error codes.
const (
	ErrorTeamPlanRequired = "team_plan_required"
	ErrorNotFound         = "recording_not_found"
	ErrorForbidden        = "recording_forbidden"
	ErrorKeyUnavailable   = "recording_key_unavailable"
	ErrorKeyPending       = "recording_key_pending"
	ErrorKeyConflict      = "recording_key_conflict" // generation is not current+1, or a bad rotation signature
	ErrorRecordingDeleted = "recording_deleted"
)

const rotationDomain = "paperboat.recording-key.rotation.v1"

// RotationMessage is the exact byte string a rotation signature covers.
func RotationMessage(teamID string, generation int64, publicKey, signingPublicKey []byte) []byte {
	out := []byte(rotationDomain)
	for _, part := range [][]byte{[]byte(teamID), binary.BigEndian.AppendUint64(nil, uint64(generation)), publicKey, signingPublicKey} {
		out = binary.BigEndian.AppendUint32(out, uint32(len(part)))
		out = append(out, part...)
	}
	return out
}

// ValidKey checks sizes and generation.
func (k Key) ValidKey() bool {
	return k.Generation >= 1 && len(k.PublicKey) == 32 && len(k.SigningPublicKey) == ed25519.PublicKeySize &&
		(k.Generation == 1) == (len(k.RotationSignature) == 0) && (k.Generation == 1 || len(k.RotationSignature) == ed25519.SignatureSize)
}

// VerifyRotation reports whether next is a valid successor of previous for team.
func VerifyRotation(teamID string, previous, next Key) bool {
	return previous.ValidKey() && next.ValidKey() && next.Generation == previous.Generation+1 &&
		ed25519.Verify(previous.SigningPublicKey, RotationMessage(teamID, next.Generation, next.PublicKey, next.SigningPublicKey), next.RotationSignature)
}
