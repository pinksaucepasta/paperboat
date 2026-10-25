package tunnelcert

// HTTP-01 is the staging-only challenge path. The key authorization for each
// pending challenge is held in memory for the order's lifetime and handed to
// the edges that serve the domain on their next ingress refresh. The edge
// answers GET /.well-known/acme-challenge/<token> for that hostname only.
// The registry is process-local, so a deployment that runs more than one
// server process must not use http-01; configuration refuses it in production.

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

const (
	http01ResponseLifetime = 10 * time.Minute
	http01PollInterval     = 100 * time.Millisecond
)

// HTTP01Record is one http-01 response: the key authorization for Token, to be
// served for Hostname by each edge in EdgeNodeIDs.
type HTTP01Record struct {
	Hostname         string
	Token            string
	KeyAuthorization string
	EdgeNodeIDs      []string
}

// HTTP01Publisher hands key authorizations to the edges. Wait returns nil only
// after every edge in the record has fetched it. Withdraw is idempotent.
type HTTP01Publisher interface {
	Publish(context.Context, HTTP01Record) error
	Wait(context.Context, HTTP01Record) error
	Withdraw(context.Context, HTTP01Record) error
}

// HTTP01Responses is the in-memory registry behind HTTP01Publisher. It is
// bounded and expires entries, so a stalled order cannot grow it.
type HTTP01Responses struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]*http01Entry
}

type http01Entry struct {
	response connectorprotocol.HTTP01Response
	expires  time.Time
	fetched  map[string]bool
}

// NewHTTP01Responses returns an empty registry. now must not be nil.
func NewHTTP01Responses(now func() time.Time) *HTTP01Responses {
	return &HTTP01Responses{now: now, entries: make(map[string]*http01Entry)}
}

func (r *HTTP01Responses) Publish(_ context.Context, record HTTP01Record) error {
	response := connectorprotocol.HTTP01Response{Hostname: record.Hostname, Token: record.Token, KeyAuthorization: record.KeyAuthorization}
	if err := response.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrDNSChallengeUnavailable, err)
	}
	if len(record.EdgeNodeIDs) == 0 {
		return fmt.Errorf("%w: HTTP-01 has no edge to answer", ErrDNSChallengeUnavailable)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.purgeLocked(now)
	if _, exists := r.entries[record.Token]; !exists && len(r.entries) >= connectorprotocol.MaxHTTP01Responses {
		return fmt.Errorf("%w: too many pending HTTP-01 challenges", ErrDNSChallengeUnavailable)
	}
	fetched := make(map[string]bool, len(record.EdgeNodeIDs))
	for _, id := range record.EdgeNodeIDs {
		if !validIdentifier(id) {
			return fmt.Errorf("%w: HTTP-01 edge identifier is invalid", ErrDNSChallengeUnavailable)
		}
		fetched[id] = false
	}
	r.entries[record.Token] = &http01Entry{
		response: response,
		expires:  now.Add(http01ResponseLifetime),
		fetched:  fetched,
	}
	return nil
}

// Wait polls until every edge has fetched the response. It is bounded by ctx,
// which the issuer bounds by its propagation timeout.
func (r *HTTP01Responses) Wait(ctx context.Context, record HTTP01Record) error {
	ticker := time.NewTicker(http01PollInterval)
	defer ticker.Stop()
	for {
		done, err := r.allFetched(record.Token)
		if err != nil || done {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: edge has not fetched the HTTP-01 response", ErrDNSChallengePending)
		case <-ticker.C:
		}
	}
}

func (r *HTTP01Responses) allFetched(token string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[token]
	if !ok || !entry.expires.After(r.now()) {
		return false, fmt.Errorf("%w: HTTP-01 response was withdrawn", ErrDNSChallengeUnavailable)
	}
	for _, fetched := range entry.fetched {
		if !fetched {
			return false, nil
		}
	}
	return true, nil
}

func (r *HTTP01Responses) Withdraw(_ context.Context, record HTTP01Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, record.Token)
	return nil
}

// ForEdge returns the unexpired responses that edge must serve and marks them
// fetched. It is called from the authenticated ingress snapshot.
func (r *HTTP01Responses) ForEdge(nodeID string) []connectorprotocol.HTTP01Response {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.purgeLocked(r.now())
	var out []connectorprotocol.HTTP01Response
	for _, entry := range r.entries {
		if _, targeted := entry.fetched[nodeID]; !targeted {
			continue
		}
		entry.fetched[nodeID] = true
		out = append(out, entry.response)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Token < out[b].Token })
	return out
}

func (r *HTTP01Responses) purgeLocked(now time.Time) {
	for token, entry := range r.entries {
		if !entry.expires.After(now) {
			delete(r.entries, token)
		}
	}
}

var _ HTTP01Publisher = (*HTTP01Responses)(nil)
