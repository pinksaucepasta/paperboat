package preview

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
)

// Foreground lease handover: a pb update starts the next worker generation
// while the current one still serves. Each foreground lease is kept in one
// owner-only file, its token only as SHA-256, so the next generation registers
// the same owner session at start and resumes the preview at once; the CLI's
// next heartbeat, to whichever generation accepts it, proves the token (pbvm4
// feat8: previews and tunnels paused ~9 s until the CLI reattached).
// Prior art: Envoy hot restart hands shared state to the new process before
// the old one drains.

const handoverLeaseSchema = "paperboat.preview-owner-lease-handover/v1"

type handoverLease struct {
	Schema         string      `json:"schema"`
	ID             string      `json:"id"`
	MachineID      string      `json:"machine_id"`
	OwnerSessionID string      `json:"owner_session_id"`
	Target         LeaseTarget `json:"target"`
	ExpiresAt      time.Time   `json:"expires_at"`
	TokenSHA256    string      `json:"token_sha256"`
}

// tokenMatches proves the lease token. A taken-over lease knows only its hash
// and learns the token from the first holder that proves it.
func (e *ownerSessionLeaseEntry) tokenMatches(token string) bool {
	token = strings.TrimSpace(token)
	if e.lease.Token != "" {
		return secureTokenEqual(e.lease.Token, token)
	}
	sum := sha256.Sum256([]byte(token))
	if token == "" || subtle.ConstantTimeCompare(sum[:], e.tokenHash[:]) != 1 {
		return false
	}
	e.lease.Token = token
	return true
}

func (m *OwnerSessionLeaseManager) handoverPath(leaseID string) string {
	return filepath.Join(m.handoverDir, leaseID+".json")
}

// handOverLocked records a live foreground lease for the next generation.
// shortcut: one small write per heartbeat (every few seconds per foreground
// preview); batch them if the lease limit grows past hundreds.
func (m *OwnerSessionLeaseManager) handOverLocked(entry *ownerSessionLeaseEntry) {
	if m.handoverDir == "" || entry.lease.Background {
		return
	}
	data, err := json.Marshal(handoverLease{
		Schema: handoverLeaseSchema, ID: entry.lease.ID, MachineID: m.machineID, OwnerSessionID: entry.lease.OwnerSessionID,
		Target: entry.lease.Target, ExpiresAt: entry.lease.ExpiresAt.UTC(), TokenSHA256: hex.EncodeToString(entry.tokenHash[:]),
	})
	if err == nil {
		err = atomicfile.Write(m.handoverPath(entry.lease.ID), data, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1})
	}
	if err != nil {
		leaseStepFailed("handover_record", err)
	}
}

// forgetHandoverLocked removes the record; with a non-zero now only when no
// other generation renewed it past now.
func (m *OwnerSessionLeaseManager) forgetHandoverLocked(entry *ownerSessionLeaseEntry, now time.Time) {
	if m.handoverDir == "" {
		return
	}
	if !now.IsZero() {
		if stored, ok := m.readHandover(entry.lease.ID); ok && stored.ExpiresAt.After(now) {
			return
		}
	}
	_ = os.Remove(m.handoverPath(entry.lease.ID))
}

// refreshFromHandoverLocked keeps a lease that the other generation renewed:
// during the overlap the CLI's heartbeats reach either one.
func (m *OwnerSessionLeaseManager) refreshFromHandoverLocked(entry *ownerSessionLeaseEntry, now time.Time) bool {
	if m.handoverDir == "" || entry.lease.Background {
		return false
	}
	stored, ok := m.readHandover(entry.lease.ID)
	if !ok || !stored.ExpiresAt.After(now) || stored.OwnerSessionID != entry.lease.OwnerSessionID {
		return false
	}
	entry.lease.ExpiresAt = stored.ExpiresAt
	return true
}

func (m *OwnerSessionLeaseManager) readHandover(leaseID string) (handoverLease, bool) {
	data, err := os.ReadFile(m.handoverPath(leaseID))
	if err != nil || len(data) > ownerSessionLeaseBodyLimit {
		return handoverLease{}, false
	}
	var stored handoverLease
	if json.Unmarshal(data, &stored) != nil || stored.Schema != handoverLeaseSchema || stored.ID != leaseID || stored.MachineID != m.machineID ||
		!validLeaseID(stored.OwnerSessionID) || validateLeaseTarget(stored.Target) != nil {
		return handoverLease{}, false
	}
	return stored, true
}

// takeOverForegroundLeases registers every unexpired recorded lease before
// this generation serves; expired or unreadable records are removed.
func (m *OwnerSessionLeaseManager) takeOverForegroundLeases() error {
	if m.handoverDir == "" {
		return nil
	}
	if err := os.MkdirAll(m.handoverDir, 0o700); err != nil {
		return errors.Join(ErrOwnerSessionLeaseInvalid, err)
	}
	entries, err := os.ReadDir(m.handoverDir)
	if err != nil {
		return errors.Join(ErrOwnerSessionLeaseInvalid, err)
	}
	now := m.now().UTC()
	for _, file := range entries {
		leaseID, ok := strings.CutSuffix(file.Name(), ".json")
		if !ok || !validLeaseID(leaseID) || len(m.leases) >= m.max {
			continue
		}
		stored, ok := m.readHandover(leaseID)
		hash, hashErr := hex.DecodeString(stored.TokenSHA256)
		if !ok || hashErr != nil || len(hash) != sha256.Size || !stored.ExpiresAt.After(now) || stored.ExpiresAt.Sub(now) > m.ttl {
			_ = os.Remove(m.handoverPath(leaseID))
			continue
		}
		if _, _, err := m.registry.RegisterMachineOwnerSession(m.machineID, stored.OwnerSessionID, stored.Target); err != nil {
			continue
		}
		entry := &ownerSessionLeaseEntry{lease: OwnerSessionLease{
			Schema: OwnerSessionLeaseSchema, ID: leaseID, MachineID: m.machineID, OwnerSessionID: stored.OwnerSessionID,
			Target: stored.Target, ExpiresAt: stored.ExpiresAt,
		}}
		copy(entry.tokenHash[:], hash)
		m.leases[leaseID] = entry
	}
	return nil
}
