// Package recordingcontrol is the runtime half of team session recordings: it holds the machine's
// recording policy, decides who may start and stop a recording, starts the recordings admin rules
// ask for, and reports every recording to Paperboat through the host's audit queue. The session
// owner (hostruntime/recordings) does the recording itself.
package recordingcontrol

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	wire "github.com/pinksaucepasta/paperboat/shared/recording"
	"github.com/pinksaucepasta/paperboat/shared/wirejson"
)

const policyLimit = 256 << 10

// Fetcher reads the machine's recording policy with the machine's own credential.
type Fetcher struct {
	ServerURL string
	Client    *http.Client
	Tokens    interface {
		Token(context.Context) (string, error)
	}
	Proofs interface {
		Proof(context.Context, string, string, string, []byte) ([]byte, error)
	}
	OperationID func() (string, error)
}

func (f *Fetcher) Fetch(ctx context.Context) (wire.Policy, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, f.ServerURL+wire.PolicyPath, nil)
	if err != nil {
		return wire.Policy{}, err
	}
	token, err := f.Tokens.Token(ctx)
	if err != nil {
		return wire.Policy{}, fmt.Errorf("recording policy: runtime credential: %w", err)
	}
	operationID, err := f.OperationID()
	if err != nil {
		return wire.Policy{}, err
	}
	proof, err := f.Proofs.Proof(ctx, operationID, http.MethodGet, wire.PolicyPath, nil)
	if err != nil {
		return wire.Policy{}, fmt.Errorf("recording policy: machine proof: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	response, err := f.Client.Do(request)
	if err != nil {
		return wire.Policy{}, fmt.Errorf("recording policy: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, policyLimit+1))
	if err != nil {
		return wire.Policy{}, fmt.Errorf("recording policy: read the answer: %w", err)
	}
	if response.StatusCode != http.StatusOK || len(body) > policyLimit {
		return wire.Policy{}, fmt.Errorf("recording policy: the server answered %d", response.StatusCode)
	}
	var policy wire.Policy
	if err := wirejson.Decode(body, &policy); err != nil {
		return wire.Policy{}, fmt.Errorf("recording policy: %w", err)
	}
	return policy, nil
}

// cached is what the machine keeps across restarts: the last policy and the key it pinned. The pin
// stays even when the policy is replaced, so the server can never swap the team key unsigned.
type cached struct {
	Policy     wire.Policy `json:"policy"`
	Pinned     *wire.Key   `json:"pinned,omitempty"`
	PinnedTeam string      `json:"pinned_team,omitempty"`
}

// pin is the key pinned for policy's team; a machine that moved to another team pins afresh.
func (c cached) pin(policy wire.Policy) *wire.Key {
	if c.PinnedTeam != policy.TeamID {
		return nil
	}
	return c.Pinned
}

// Policy is the current recording policy and the team key this machine verified.
type Policy struct {
	wire.Policy
	Key *wire.Key // nil: no verified key, so nothing records
}

func (p Policy) Admin(accountID string) bool { return contains(p.AdminAccounts, accountID) }

func (p Policy) Records(accountID string) bool { return contains(p.RecordAccounts, accountID) }

// Usable reports whether this machine may record at all.
func (p Policy) Usable() bool {
	return p.Enabled && p.Key != nil && p.RetentionDays > 0 && p.DiskQuotaMB > 0
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if value != "" && candidate == value {
			return true
		}
	}
	return false
}

// PolicyStore owns the cached policy and the key pin.
type PolicyStore struct {
	path    string
	mu      sync.Mutex
	current cached
}

func OpenPolicyStore(path string) (*PolicyStore, error) {
	s := &PolicyStore{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the cached recording policy: %w", err)
	}
	if json.Unmarshal(data, &s.current) != nil {
		// A torn cache is refetched; the pin goes with it only if it cannot be read.
		s.current = cached{}
	}
	return s, nil
}

// Current returns the cached policy with its verified key.
func (s *PolicyStore) Current() Policy {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, _ := verifiedKey(s.current.Policy, s.current.pin(s.current.Policy))
	return Policy{Policy: s.current.Policy, Key: key}
}

// Replace keeps a newly fetched policy. It pins the oldest key of the chain on first use, and the
// newest key is used only when every rotation from the pin to it is signed by its predecessor.
func (s *PolicyStore) Replace(policy wire.Policy) (Policy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if policy.Version < s.current.Policy.Version {
		key, _ := verifiedKey(s.current.Policy, s.current.pin(s.current.Policy))
		return Policy{Policy: s.current.Policy, Key: key}, nil // an older answer never replaces a newer one
	}
	next := cached{Policy: policy, Pinned: s.current.pin(policy), PinnedTeam: policy.TeamID}
	key, err := verifiedKey(policy, next.Pinned)
	if next.Pinned == nil && key != nil {
		first := policy.Keys[0]
		next.Pinned = &first
	}
	encoded, marshalErr := json.Marshal(next)
	if marshalErr != nil {
		return Policy{}, marshalErr
	}
	if writeErr := atomicfile.Write(s.path, encoded, atomicfile.CurrentOwnerOptions(0o600)); writeErr != nil {
		return Policy{}, fmt.Errorf("cache the recording policy: %w", writeErr)
	}
	s.current = next
	return Policy{Policy: policy, Key: key}, err
}

var ErrKeyUnverified = errors.New("the team recording key does not follow the key this machine pinned")

// verifiedKey walks the chain from the pin (or, before any pin, from the chain's first key) and
// returns its newest key, or nil when the chain is empty or broken.
func verifiedKey(policy wire.Policy, pinned *wire.Key) (*wire.Key, error) {
	keys := policy.Keys
	if len(keys) == 0 {
		return nil, nil
	}
	start := 0
	if pinned != nil {
		start = -1
		for i, key := range keys {
			if key.Generation == pinned.Generation {
				start = i
			}
		}
		if start < 0 || !bytes.Equal(keys[start].PublicKey, pinned.PublicKey) || !bytes.Equal(keys[start].SigningPublicKey, pinned.SigningPublicKey) {
			return nil, ErrKeyUnverified
		}
	}
	if !keys[start].ValidKey() {
		return nil, ErrKeyUnverified
	}
	for i := start + 1; i < len(keys); i++ {
		if !wire.VerifyRotation(policy.TeamID, keys[i-1], keys[i]) {
			return nil, ErrKeyUnverified
		}
	}
	newest := keys[len(keys)-1]
	return &newest, nil
}
