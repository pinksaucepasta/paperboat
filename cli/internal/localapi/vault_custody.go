package localapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

const VaultCustodyLifetime = 15 * time.Minute
const vaultCustodyLeaseLifetime = 2 * time.Minute
const vaultCustodyMaximumEntries = 4
const vaultCustodyMaximumBytes = 64 << 20
const vaultCustodyMaximumValueBytes = config.PasswordVaultRecordMaximumBytes
const vaultCustodyReferencePrefix = "environment-password-vault-v1-"

var ErrVaultCustodyStale = errors.New("ENV vault memory custody changed; unlock again")
var ErrVaultCustodyInvalid = errors.New("invalid ENV vault memory custody request")
var ErrVaultCustodyLimit = errors.New("ENV vault memory custody limit reached")

type VaultCustodyRequest struct {
	Reference  string `json:"reference"`
	Operation  string `json:"operation"`
	Generation uint64 `json:"generation"`
	Value      []byte `json:"value,omitempty"`
	Unlock     bool   `json:"unlock,omitempty"`
	Lease      string `json:"lease,omitempty"`
	SessionID  string `json:"session_id"`
}
type VaultCustodyResponse struct {
	Generation uint64    `json:"generation"`
	Value      []byte    `json:"value,omitempty"`
	ExpiresAt  time.Time `json:"expires_at"`
	Lease      string    `json:"lease,omitempty"`
}
type vaultCustodyEntry struct {
	record       config.PasswordVaultRecord
	size         int
	present      bool
	generation   uint64
	session      string
	expires      time.Time
	lease        string
	leaseExpires time.Time
	changed      chan struct{}
}

// VaultCustodyStore belongs only to the user daemon. No persistence, runtime
// projection, diagnostics or background refresh can obtain its key payloads.
type VaultCustodyStore struct {
	mu       sync.Mutex
	entries  map[string]*vaultCustodyEntry
	timer    *time.Timer
	ttl      time.Duration
	leaseTTL time.Duration
	closed   bool
}

func NewVaultCustodyStore() *VaultCustodyStore {
	return newVaultCustodyStore(VaultCustodyLifetime, vaultCustodyLeaseLifetime)
}
func newVaultCustodyStore(ttl, leaseTTL time.Duration) *VaultCustodyStore {
	return &VaultCustodyStore{entries: make(map[string]*vaultCustodyEntry), ttl: ttl, leaseTTL: leaseTTL}
}
func validVaultCustodyReference(ref string) bool {
	if !strings.HasPrefix(ref, vaultCustodyReferencePrefix) {
		return false
	}
	suffix := strings.TrimPrefix(ref, vaultCustodyReferencePrefix)
	if len(suffix) != 32 {
		return false
	}
	for _, c := range suffix {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func validVaultCustodySession(session string) bool {
	if len(session) < 1 || len(session) > 256 {
		return false
	}
	for _, c := range session {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
func clearVaultCustodyRecord(record *config.PasswordVaultRecord) {
	record.Clear()
	clear(record.Envelope)
	if record.Pending != nil {
		clear(record.Pending.Envelope)
	}
	if record.Operation != nil {
		clear(record.Operation.Request)
	}
	*record = config.PasswordVaultRecord{}
}
func (s *VaultCustodyStore) invalidateLeaseLocked(e *vaultCustodyEntry) {
	e.lease = ""
	e.leaseExpires = time.Time{}
	close(e.changed)
	e.changed = make(chan struct{})
}
func (s *VaultCustodyStore) lockEntryLocked(e *vaultCustodyEntry) {
	e.record.Clear()
	e.expires = time.Time{}
	e.generation++
	s.invalidateLeaseLocked(e)
}
func (s *VaultCustodyStore) expireLocked(now time.Time) {
	for _, e := range s.entries {
		if !e.expires.IsZero() && !now.Before(e.expires) {
			s.lockEntryLocked(e)
		} else if !e.leaseExpires.IsZero() && !now.Before(e.leaseExpires) {
			e.generation++
			s.invalidateLeaseLocked(e)
		}
	}
}
func (s *VaultCustodyStore) scheduleLocked() {
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	if s.closed {
		return
	}
	var next time.Time
	for _, e := range s.entries {
		for _, deadline := range []time.Time{e.expires, e.leaseExpires} {
			if !deadline.IsZero() && (next.IsZero() || deadline.Before(next)) {
				next = deadline
			}
		}
	}
	if next.IsZero() {
		return
	}
	s.timer = time.AfterFunc(time.Until(next), func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return
		}
		s.expireLocked(time.Now())
		s.scheduleLocked()
	})
}
func (s *VaultCustodyStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	for _, e := range s.entries {
		clearVaultCustodyRecord(&e.record)
		e.size = 0
		e.present = false
		e.generation++
		s.invalidateLeaseLocked(e)
	}
	return nil
}
func (s *VaultCustodyStore) Handle(ctx context.Context, req VaultCustodyRequest) (VaultCustodyResponse, error) {
	if ctx == nil || !validVaultCustodyReference(req.Reference) || !validVaultCustodySession(req.SessionID) || len(req.Value) > vaultCustodyMaximumValueBytes || req.Unlock && req.Operation != "put" || len(req.Value) > 0 && req.Operation != "put" {
		return VaultCustodyResponse{}, ErrVaultCustodyInvalid
	}
	for {
		if err := ctx.Err(); err != nil {
			return VaultCustodyResponse{}, err
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return VaultCustodyResponse{}, ErrVaultCustodyStale
		}
		if err := ctx.Err(); err != nil {
			s.mu.Unlock()
			return VaultCustodyResponse{}, err
		}
		s.expireLocked(time.Now())
		e := s.entries[req.Reference]
		if e == nil {
			if req.Operation != "get" && req.Operation != "begin" {
				s.mu.Unlock()
				return VaultCustodyResponse{}, ErrVaultCustodyStale
			}
			if len(s.entries) >= vaultCustodyMaximumEntries {
				s.mu.Unlock()
				return VaultCustodyResponse{}, ErrVaultCustodyLimit
			}
			var seed [8]byte
			if _, err := rand.Read(seed[:]); err != nil {
				s.mu.Unlock()
				return VaultCustodyResponse{}, ErrVaultCustodyInvalid
			}
			generation := binary.BigEndian.Uint64(seed[:]) & ((1 << 52) - 1)
			clear(seed[:])
			if generation == 0 {
				generation = 1
			}
			e = &vaultCustodyEntry{generation: generation, session: req.SessionID, changed: make(chan struct{})}
			s.entries[req.Reference] = e
		}
		if e.session != req.SessionID {
			if req.Operation != "get" && req.Operation != "begin" {
				s.mu.Unlock()
				return VaultCustodyResponse{}, ErrVaultCustodyStale
			}
			clearVaultCustodyRecord(&e.record)
			e.size = 0
			e.present = false
			e.session = req.SessionID
			s.lockEntryLocked(e)
		}
		if req.Lease != "" && req.Lease != e.lease {
			s.mu.Unlock()
			return VaultCustodyResponse{}, ErrVaultCustodyStale
		}
		if req.Operation == "begin" && e.lease != "" {
			changed := e.changed
			s.scheduleLocked()
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return VaultCustodyResponse{}, ctx.Err()
			case <-changed:
				continue
			}
		}
		response, err := s.handleLocked(ctx, e, req)
		s.scheduleLocked()
		s.mu.Unlock()
		return response, err
	}
}
func (s *VaultCustodyStore) handleLocked(ctx context.Context, e *vaultCustodyEntry, req VaultCustodyRequest) (VaultCustodyResponse, error) {
	response := func() (VaultCustodyResponse, error) {
		out := VaultCustodyResponse{Generation: e.generation, ExpiresAt: e.expires}
		if req.Lease != "" {
			out.Lease = e.lease
		}
		if e.present {
			raw, err := json.Marshal(struct {
				Version int `json:"version"`
				config.PasswordVaultRecord
			}{1, e.record})
			if err != nil {
				return VaultCustodyResponse{}, ErrVaultCustodyInvalid
			}
			out.Value = raw
		}
		return out, nil
	}
	switch req.Operation {
	case "get":
		return response()
	case "begin":
		if req.Lease != "" {
			return VaultCustodyResponse{}, ErrVaultCustodyInvalid
		}
		var token [32]byte
		if _, err := rand.Read(token[:]); err != nil {
			return VaultCustodyResponse{}, ErrVaultCustodyInvalid
		}
		e.lease = hex.EncodeToString(token[:])
		clear(token[:])
		e.leaseExpires = time.Now().Add(s.leaseTTL)
		if !e.expires.IsZero() && e.expires.Before(e.leaseExpires) {
			e.leaseExpires = e.expires
		}
		return VaultCustodyResponse{Generation: e.generation, ExpiresAt: e.expires, Lease: e.lease}, nil
	case "end":
		if req.Lease == "" {
			return VaultCustodyResponse{}, ErrVaultCustodyStale
		}
		s.invalidateLeaseLocked(e)
		return VaultCustodyResponse{Generation: e.generation, ExpiresAt: e.expires}, nil
	case "lock":
		s.lockEntryLocked(e)
		return VaultCustodyResponse{Generation: e.generation}, nil
	case "remove":
		clearVaultCustodyRecord(&e.record)
		e.size = 0
		e.present = false
		s.lockEntryLocked(e)
		return VaultCustodyResponse{Generation: e.generation}, nil
	case "put":
		if req.Generation != e.generation || e.lease != "" && req.Lease != e.lease {
			return VaultCustodyResponse{}, ErrVaultCustodyStale
		}
		record, err := validateVaultCustodyRecord(req.Reference, req.Value)
		if err != nil {
			return VaultCustodyResponse{}, ErrVaultCustodyInvalid
		}
		accepted := false
		defer func() {
			if !accepted {
				clearVaultCustodyRecord(&record)
			}
		}()
		if err := ctx.Err(); err != nil {
			return VaultCustodyResponse{}, err
		}
		s.expireLocked(time.Now())
		if req.Generation != e.generation || req.Lease != "" && req.Lease != e.lease || e.lease != "" && req.Lease != e.lease {
			return VaultCustodyResponse{}, ErrVaultCustodyStale
		}
		hasPayload := len(record.Payload) > 0 || record.Pending != nil && len(record.Pending.Payload) > 0
		if req.Unlock && !hasPayload {
			return VaultCustodyResponse{}, ErrVaultCustodyInvalid
		}
		if hasPayload && !req.Unlock && e.expires.IsZero() {
			return VaultCustodyResponse{}, ErrVaultCustodyStale
		}
		total := len(req.Value)
		for _, other := range s.entries {
			if other != e {
				total += other.size
			}
		}
		if total > vaultCustodyMaximumBytes {
			return VaultCustodyResponse{}, ErrVaultCustodyLimit
		}
		clearVaultCustodyRecord(&e.record)
		e.record = record
		e.present = true
		e.size = len(req.Value)
		e.generation++
		accepted = true
		if req.Unlock {
			e.expires = time.Now().Add(s.ttl)
		}
		if !hasPayload {
			e.expires = time.Time{}
		}
		return VaultCustodyResponse{Generation: e.generation, ExpiresAt: e.expires, Lease: e.lease}, nil
	default:
		return VaultCustodyResponse{}, ErrVaultCustodyInvalid
	}
}

type vaultValidationMemory struct {
	ref   string
	value []byte
}

func (*vaultValidationMemory) EnvironmentSecureStore() {}
func (s *vaultValidationMemory) Get(ref string) (string, error) {
	if ref != s.ref {
		return "", config.ErrSecretNotFound
	}
	return string(s.value), nil
}
func (s *vaultValidationMemory) Set(ref, value string) error { return ErrVaultCustodyInvalid }
func (s *vaultValidationMemory) Delete(ref string) error     { return ErrVaultCustodyInvalid }
func validateVaultCustodyRecord(ref string, value []byte) (config.PasswordVaultRecord, error) {
	if len(value) == 0 || len(value) > vaultCustodyMaximumValueBytes {
		return config.PasswordVaultRecord{}, ErrVaultCustodyInvalid
	}
	var record struct {
		Version int `json:"version"`
		config.PasswordVaultRecord
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || record.Version != 1 {
		clearVaultCustodyRecord(&record.PasswordVaultRecord)
		return config.PasswordVaultRecord{}, ErrVaultCustodyInvalid
	}
	issuer, account := record.Head.Issuer, record.Head.AccountID
	if issuer == "" && record.Pending != nil {
		issuer, account = record.Pending.Head.Issuer, record.Pending.Head.AccountID
	}
	clearVaultCustodyRecord(&record.PasswordVaultRecord)
	store := config.ProfileStore{Path: "memory-only", Secrets: &vaultValidationMemory{ref: ref, value: value}}
	return store.LoadPasswordVault(issuer, account)
}
