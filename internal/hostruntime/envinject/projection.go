package envinject

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/environmentkey"
)

const ProjectionObservationSchema = "paperboat.environment-projection-observation/v1"
const ProjectionBundleSchema = "paperboat.environment-projection-bundle/v1"
const maximumProjectionStateBytes = 1 << 20

var ErrProjectionWriterChanged = errors.New("ENV projection writer binding changed; authenticate the current host binding")

type ProjectionCursor struct {
	Revision   uint64 `json:"revision"`
	DocumentID string `json:"document_id"`
}
type ProjectionObservation struct {
	Schema             string            `json:"schema"`
	ObservationSeq     uint64            `json:"observation_seq"`
	HostRecipientKeyID string            `json:"host_recipient_key_id"`
	Projection         *ProjectionCursor `json:"projection"`
	FenceGeneration    uint64            `json:"fence_generation"`
	State              string            `json:"state"`
	ErrorCode          *string           `json:"error_code"`
	ObservedAt         time.Time         `json:"observed_at"`
}
type ProjectionBundle = environmente2ee.ProjectionBundle

type ProjectionConfig struct {
	Path, HighWaterPath, Issuer, AccountID, MachineID string
	InstallationGeneration, HostKeyGeneration         uint64
	HostPublic                                        []byte
	WriterPublic                                      []byte
	Keys                                              environmentkey.Source
	GenesisMarker                                     environmentkey.GenesisMarker
}
type projectionFloor struct {
	Schema                 string `json:"schema"`
	AccountID              string `json:"account_id"`
	MachineID              string `json:"machine_id"`
	InstallationGeneration uint64 `json:"installation_generation"`
	HostKeyGeneration      uint64 `json:"host_key_generation"`
	HostPublic             string `json:"host_public"`
	WriterPublic           string `json:"writer_public"`
	Fence                  uint64 `json:"fence"`
	Selection              uint64 `json:"selection"`
	Revision               uint64 `json:"revision"`
	DocumentID             string `json:"document_id"`
	ObservationSeq         uint64 `json:"observation_seq"`
	State                  string `json:"state"`
}
type ProjectionStore struct {
	mu        sync.RWMutex
	config    ProjectionConfig
	integrity []byte
	floor     projectionFloor
	bundle    ProjectionBundle
	values    map[string][]byte
	state     string
	closed    bool
	write     func(string, []byte, any) error
}

func OpenProjection(ctx context.Context, c ProjectionConfig) (*ProjectionStore, error) {
	if ctx == nil || !filepath.IsAbs(c.Path) || !filepath.IsAbs(c.HighWaterPath) || c.Path == c.HighWaterPath || !validIdentifier(c.AccountID) || !validIdentifier(c.MachineID) || c.Issuer == "" || c.InstallationGeneration == 0 || c.HostKeyGeneration == 0 || len(c.HostPublic) != 32 || len(c.WriterPublic) != 32 || c.Keys == nil || c.GenesisMarker == nil {
		return nil, ErrInvalidSnapshot
	}
	material, err := c.Keys.Load(ctx)
	if err != nil {
		return nil, err
	}
	defer material.Destroy()
	public, err := material.Public()
	if err != nil || material.Generation != c.HostKeyGeneration || !bytes.Equal(public[:], c.HostPublic) {
		return nil, ErrInvalidSnapshot
	}
	integrity, err := material.StateIntegrityKey()
	if err != nil {
		return nil, err
	}
	defer clear(integrity[:])
	s := &ProjectionStore{config: c, integrity: bytes.Clone(integrity[:]), state: "pending", write: writeProjectionState}
	s.floor = projectionFloor{Schema: "paperboat.environment-projection-high-water/v1", AccountID: c.AccountID, MachineID: c.MachineID, InstallationGeneration: c.InstallationGeneration, HostKeyGeneration: c.HostKeyGeneration, HostPublic: base64.RawURLEncoding.EncodeToString(c.HostPublic), WriterPublic: base64.RawURLEncoding.EncodeToString(c.WriterPublic)}
	genesis, err := c.GenesisMarker.GenesisState()
	if err != nil {
		return nil, errors.Join(ErrObservationLost, err)
	}
	var floor projectionFloor
	err = readProjectionState(c.HighWaterPath, s.integrity, &floor)
	if errors.Is(err, os.ErrNotExist) {
		if genesis != environmentkey.GenesisFresh && genesis != environmentkey.GenesisPending {
			return nil, ErrObservationLost
		}
		if _, cacheErr := os.Lstat(c.Path); !errors.Is(cacheErr, os.ErrNotExist) {
			return nil, ErrObservationLost
		}
		if err := c.GenesisMarker.PrepareGenesis(); err != nil {
			return nil, err
		}
		if err := s.write(c.HighWaterPath, s.integrity, s.floor); err != nil {
			return nil, err
		}
		if err := c.GenesisMarker.CommitGenesis(); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, errors.Join(ErrObservationLost, err)
	} else {
		if floor.Schema != s.floor.Schema || floor.AccountID != c.AccountID || floor.MachineID != c.MachineID || floor.InstallationGeneration != c.InstallationGeneration || floor.HostKeyGeneration != c.HostKeyGeneration || floor.HostPublic != s.floor.HostPublic || floor.ObservationSeq > environmente2ee.MaximumContractInteger || floor.Fence > environmente2ee.MaximumContractInteger || floor.Revision > environmente2ee.MaximumContractInteger || floor.Selection > environmente2ee.MaximumContractInteger {
			return nil, ErrObservationLost
		}
		s.floor = floor
		if genesis == environmentkey.GenesisPending {
			if err := c.GenesisMarker.CommitGenesis(); err != nil {
				return nil, err
			}
		} else if genesis != environmentkey.GenesisEstablished {
			return nil, ErrObservationLost
		}
	}
	var cached ProjectionBundle
	if err := readProjectionState(c.Path, s.integrity, &cached); err == nil && s.matchesFloor(cached) {
		values, err := s.verify(ctx, cached, false)
		if err == nil {
			s.bundle = cached
			s.values = values
			s.state = cached.State
		}
	}
	// A missing/interrupted cache never lowers its separately authenticated floor.
	// Heartbeat can fetch the same or a newer head to recover legitimate readiness.
	return s, nil
}
func (s *ProjectionStore) matchesIdentity(b ProjectionBundle) bool {
	return b.Schema == ProjectionBundleSchema && b.AccountID == s.config.AccountID && b.MachineID == s.config.MachineID && b.InstallationGeneration == s.config.InstallationGeneration && b.HostKeyGeneration == s.config.HostKeyGeneration && b.HostPublic == s.floor.HostPublic
}
func (s *ProjectionStore) matchesFloor(b ProjectionBundle) bool {
	return s.matchesIdentity(b) && b.FenceGeneration == s.floor.Fence && b.SelectionGeneration == s.floor.Selection && b.ProjectionRevision == s.floor.Revision && b.DocumentID == s.floor.DocumentID && b.WriterPublic == s.floor.WriterPublic
}
func (s *ProjectionStore) verify(ctx context.Context, b ProjectionBundle, allowBinding bool) (map[string][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	writer, writerErr := base64.RawURLEncoding.Strict().DecodeString(b.WriterPublic)
	if writerErr != nil || len(writer) != 32 || (b.ProjectionRevision == 0 && b.DocumentID != "") || (b.ProjectionRevision > 0 && !validManifestID(b.DocumentID)) {
		return nil, ErrInvalidSnapshot
	}
	if !s.matchesIdentity(b) || b.FenceGeneration < s.floor.Fence || b.SelectionGeneration < s.floor.Selection || b.ProjectionRevision < s.floor.Revision || b.FenceGeneration > environmente2ee.MaximumContractInteger || b.SelectionGeneration > environmente2ee.MaximumContractInteger || b.ProjectionRevision > environmente2ee.MaximumContractInteger {
		return nil, ErrInvalidSnapshot
	}
	if b.ProjectionRevision == s.floor.Revision && b.DocumentID != s.floor.DocumentID {
		return nil, ErrInvalidSnapshot
	}
	if b.State == "ready" && b.ProjectionRevision == s.floor.Revision && (s.floor.State == "pending" || s.floor.State == "revoked") {
		return nil, ErrInvalidSnapshot
	}
	if b.WriterPublic != s.floor.WriterPublic {
		if b.SelectionGeneration <= s.floor.Selection {
			return nil, ErrInvalidSnapshot
		}
		if !allowBinding {
			return nil, ErrProjectionWriterChanged
		}
	}
	if b.State == "pending" || b.State == "revoked" {
		if b.Envelope != "" {
			return nil, ErrInvalidSnapshot
		}
		return nil, nil
	}
	if b.State != "ready" || b.ProjectionRevision == 0 || b.SelectionGeneration == 0 || b.FenceGeneration == 0 || len(b.Envelope) > base64.RawURLEncoding.EncodedLen(environmente2ee.MaximumScopeBytes+65536) {
		return nil, ErrInvalidSnapshot
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(b.Envelope)
	if err != nil {
		return nil, ErrInvalidSnapshot
	}
	projection, err := environmente2ee.ParseHostProjection(raw, writer)
	if err != nil || projection.ID.String() != b.DocumentID {
		return nil, ErrInvalidSnapshot
	}
	claims := projection.Claims
	if claims.Issuer != s.config.Issuer || claims.OwnerAccount != b.AccountID || claims.MachineID != b.MachineID || claims.InstallationGeneration != b.InstallationGeneration || claims.HostKeyGeneration != b.HostKeyGeneration || !bytes.Equal(claims.HostPublic, s.config.HostPublic) || claims.SelectionGeneration != b.SelectionGeneration || claims.Revision != b.ProjectionRevision {
		return nil, ErrInvalidSnapshot
	}
	material, err := s.config.Keys.Load(ctx)
	if err != nil {
		return nil, err
	}
	defer material.Destroy()
	if material.Generation != b.HostKeyGeneration {
		return nil, ErrInvalidSnapshot
	}
	return environmente2ee.OpenHostProjection(ctx, projection, claims, material.Private[:])
}
func (s *ProjectionStore) Apply(ctx context.Context, b ProjectionBundle) error {
	return s.apply(ctx, b, false)
}

// Bind accepts a response from authenticated machine-control registration after
// explicit reprovisioning. Ordinary ciphertext delivery cannot choose a signer.
func (s *ProjectionStore) Bind(ctx context.Context, b ProjectionBundle) error {
	return s.apply(ctx, b, true)
}
func (s *ProjectionStore) apply(ctx context.Context, b ProjectionBundle, allowBinding bool) error {
	if s == nil || ctx == nil {
		return ErrInvalidSnapshot
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrNotReady
	}
	values, err := s.verify(ctx, b, allowBinding)
	if err != nil {
		s.clearValues()
		s.state = "error"
		return err
	}
	next := s.floor
	next.WriterPublic = b.WriterPublic
	next.Fence = b.FenceGeneration
	next.Selection = b.SelectionGeneration
	next.Revision = b.ProjectionRevision
	next.DocumentID = b.DocumentID
	next.State = b.State
	// Fence first, then encrypted cache, then usable memory. A crash at either
	// write leaves no path that resurrects an older valid but revoked projection.
	s.clearValues()
	s.state = "pending"
	s.floor = next
	if err := s.write(s.config.HighWaterPath, s.integrity, next); err != nil {
		clearProjectionValues(values)
		return err
	}
	if err := s.write(s.config.Path, s.integrity, b); err != nil {
		clearProjectionValues(values)
		return err
	}
	s.bundle = b
	s.values = values
	s.state = b.State
	return nil
}
func (s *ProjectionStore) NextObservation(now time.Time) (ProjectionObservation, error) {
	if s == nil {
		return ProjectionObservation{}, ErrNotReady
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ProjectionObservation{}, ErrNotReady
	}
	if now.IsZero() || s.floor.ObservationSeq >= environmente2ee.MaximumContractInteger {
		return ProjectionObservation{}, ErrObservationLost
	}
	s.floor.ObservationSeq++
	if err := s.write(s.config.HighWaterPath, s.integrity, s.floor); err != nil {
		s.clearValues()
		s.state = "error"
		return ProjectionObservation{}, err
	}
	keyID, err := environmente2ee.KeyIDX25519(s.config.HostPublic)
	if err != nil {
		return ProjectionObservation{}, err
	}
	// The store keeps its local states so callers can distinguish a usable
	// projection (ready) from a local verification failure (error). The v1
	// server observation contract uses applied and failed for those same
	// conditions; translate only the serialized observation at this boundary.
	wireState := projectionObservationState(s.state)
	out := ProjectionObservation{Schema: ProjectionObservationSchema, ObservationSeq: s.floor.ObservationSeq, HostRecipientKeyID: keyID, FenceGeneration: s.floor.Fence, State: wireState, ObservedAt: now.UTC()}
	if s.floor.Revision > 0 {
		out.Projection = &ProjectionCursor{Revision: s.floor.Revision, DocumentID: s.floor.DocumentID}
	}
	if s.state == "error" {
		code := "projection_invalid"
		out.ErrorCode = &code
	}
	return out, nil
}

func projectionObservationState(state string) string {
	switch state {
	case "ready":
		return "applied"
	case "error":
		return "failed"
	default:
		return state
	}
}

func (s *ProjectionStore) Environment() ([]string, error) {
	if s == nil {
		return nil, ErrNotReady
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrNotReady
	}
	if s.state == "revoked" {
		return nil, ErrRevoked
	}
	if s.state != "ready" {
		return nil, ErrNotReady
	}
	names := make([]string, 0, len(s.values))
	for name := range s.values {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, name+"="+string(s.values[name]))
	}
	return out, nil
}
func (s *ProjectionStore) BindingState() BindingState {
	if s == nil {
		return BindingUnknown
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return BindingUnknown
	}
	if s.state == "revoked" {
		return BindingInactive
	}
	if s.floor.Selection > 0 {
		return BindingActive
	}
	return BindingUnknown
}
func clearProjectionValues(values map[string][]byte) {
	for _, value := range values {
		clear(value)
	}
}
func (s *ProjectionStore) clearValues() { clearProjectionValues(s.values); s.values = nil }
func (s *ProjectionStore) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearValues()
	clear(s.integrity)
	s.state = "pending"
	s.closed = true
}

type authenticatedProjectionState struct {
	Record json.RawMessage `json:"record"`
	MAC    string          `json:"mac"`
}

func writeProjectionState(path string, key []byte, value any) error {
	record, err := json.Marshal(value)
	if err != nil || len(record) > maximumProjectionStateBytes || len(key) != 32 {
		return ErrInvalidSnapshot
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("paperboat.environment.projection-state/v1\x00"))
	mac.Write(record)
	raw, err := json.Marshal(authenticatedProjectionState{Record: record, MAC: base64.RawURLEncoding.EncodeToString(mac.Sum(nil))})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return atomicfile.Write(path, raw, atomicfile.CurrentOwnerOptions(0600))
}
func readProjectionState(path string, key []byte, value any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !secureStateFile(path, info, maximumProjectionStateBytes+256) {
		return ErrInvalidSnapshot
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if rejectDuplicateJSON(raw) != nil {
		return ErrInvalidSnapshot
	}
	var envelope authenticatedProjectionState
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&envelope) != nil || dec.Decode(&struct{}{}) != io.EOF {
		return ErrInvalidSnapshot
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("paperboat.environment.projection-state/v1\x00"))
	mac.Write(envelope.Record)
	actual, err := base64.RawURLEncoding.Strict().DecodeString(envelope.MAC)
	if err != nil || !hmac.Equal(mac.Sum(nil), actual) {
		return ErrInvalidSnapshot
	}
	dec = json.NewDecoder(bytes.NewReader(envelope.Record))
	dec.DisallowUnknownFields()
	if dec.Decode(value) != nil || dec.Decode(&struct{}{}) != io.EOF {
		return ErrInvalidSnapshot
	}
	return nil
}
func DecodeProjectionBundle(raw []byte) (ProjectionBundle, error) {
	var b ProjectionBundle
	if len(raw) == 0 || len(raw) > maximumProjectionStateBytes || rejectDuplicateJSON(raw) != nil {
		return b, ErrInvalidSnapshot
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&b) != nil || dec.Decode(&struct{}{}) != io.EOF || b.Schema != ProjectionBundleSchema {
		return b, ErrInvalidSnapshot
	}
	return b, nil
}

// DecodeProjectionResponse rejects duplicate members throughout the runtime
// response while allowing unrelated fields owned by other heartbeat consumers.
func DecodeProjectionResponse(raw []byte, field string) (*ProjectionBundle, error) {
	if len(raw) == 0 || len(raw) > 8<<20 || rejectDuplicateJSON(raw) != nil {
		return nil, ErrInvalidSnapshot
	}
	var response struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &response) != nil || len(response.Data) == 0 {
		return nil, ErrInvalidSnapshot
	}
	body := response.Data
	if field != "" {
		var data map[string]json.RawMessage
		if json.Unmarshal(body, &data) != nil || data == nil {
			return nil, ErrInvalidSnapshot
		}
		body = data[field]
	}
	if len(body) == 0 || bytes.Equal(body, []byte("null")) {
		return nil, nil
	}
	bundle, err := DecodeProjectionBundle(body)
	if err != nil {
		return nil, err
	}
	return &bundle, nil
}

// OpenStoredProjection restores a previously authenticated account/writer binding
// from the host's integrity-protected floor. Network availability is not required
// to retain legitimate cached ENV; a missing floor is never treated as genesis.
func OpenStoredProjection(ctx context.Context, c ProjectionConfig) (*ProjectionStore, error) {
	if ctx == nil || c.Keys == nil {
		return nil, ErrInvalidSnapshot
	}
	material, err := c.Keys.Load(ctx)
	if err != nil {
		return nil, err
	}
	defer material.Destroy()
	integrity, err := material.StateIntegrityKey()
	if err != nil {
		return nil, err
	}
	defer clear(integrity[:])
	var floor projectionFloor
	if err := readProjectionState(c.HighWaterPath, integrity[:], &floor); err != nil {
		return nil, err
	}
	writer, err := base64.RawURLEncoding.Strict().DecodeString(floor.WriterPublic)
	if err != nil || len(writer) != 32 {
		return nil, ErrInvalidSnapshot
	}
	c.AccountID = floor.AccountID
	c.WriterPublic = writer
	return OpenProjection(ctx, c)
}
