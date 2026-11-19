package inspector

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/cli/internal/atomicfile"
)

const policyFileSchema = "paperboat.inspector-policies/v1"

// persistedPolicy is what survives a restart: which capture modes the owner switched on. Generations fence one
// grant's retrieval and are learned again from the next grant, so they are never stored.
type persistedPolicy struct {
	RequestBody  bool `json:"request_body,omitempty"`
	ResponseBody bool `json:"response_body,omitempty"`
	Raw          bool `json:"raw,omitempty"`
}

type policyFile struct {
	Schema   string                     `json:"schema"`
	Policies map[string]persistedPolicy `json:"policies"`
}

// RestorePolicies makes capture the owner enabled survive a restart: it loads the owner-only policy file at path,
// keeps the entries keep accepts (a preview that cannot resume has no use for its policy), and from then on writes
// every policy change back. Captures themselves stay in memory. An unreadable file holds nothing to restore.
func (s *Store) RestorePolicies(path string, keep func(resourceID string) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policyPath = path
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read capture policies: %w", err)
	}
	var file policyFile
	if json.Unmarshal(data, &file) != nil || file.Schema != policyFileSchema {
		return nil
	}
	for id, p := range file.Policies {
		if !validResourceID(id) || !keep(id) || len(s.policies) >= DaemonMaxRecords {
			continue
		}
		s.policyEpochs[id]++
		s.policies[id] = ResourcePolicy{Enabled: true, CaptureRequestBody: p.RequestBody, CaptureResponseBody: p.ResponseBody, CaptureRaw: p.Raw}
	}
	return s.savePoliciesLocked()
}

// Forget is Revoke for a resource whose capture setting the owner ended: its stored policy goes too.
func (s *Store) Forget(resourceID string) error {
	s.Revoke(resourceID)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.savePoliciesLocked()
}

// savePoliciesLocked writes the enabled policies; no file is kept when none is enabled.
func (s *Store) savePoliciesLocked() error {
	if s.policyPath == "" {
		return nil
	}
	file := policyFile{Schema: policyFileSchema, Policies: map[string]persistedPolicy{}}
	for id, p := range s.policies {
		if p.Enabled && !p.Revoked {
			file.Policies[id] = persistedPolicy{RequestBody: p.CaptureRequestBody, ResponseBody: p.CaptureResponseBody, Raw: p.CaptureRaw}
		}
	}
	if len(file.Policies) == 0 {
		if err := os.Remove(s.policyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove capture policies: %w", err)
		}
		return nil
	}
	data, err := json.Marshal(file)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.policyPath), 0o700); err != nil {
		return fmt.Errorf("create capture policies directory: %w", err)
	}
	if err := atomicfile.Write(s.policyPath, data, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1}); err != nil {
		return fmt.Errorf("write capture policies: %w", err)
	}
	return nil
}
