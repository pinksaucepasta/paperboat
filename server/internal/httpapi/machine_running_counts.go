package httpapi

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat-server/internal/tunnelv1"
	"github.com/pinksaucepasta/paperboat-server/internal/usermachines"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

// GET /v1/machines/running-counts: what each machine runs (open terminal sessions, active tunnels, active
// previews). The console paged all three collections (up to 2,000 previews and 2,000 tunnels) every 30 s per tab
// and silently truncated above that. The counts come from the collections' own list paths, so they are
// authorized exactly as the lists are; one traversal per account and workspace serves every tab for
// runningCountsTTL (display only, never an access decision).
const (
	runningCountsTTL     = 10 * time.Second
	runningCountsMaxRows = 10_000 // per kind; past it the answer says truncated
	runningCountsMaxKeys = 10_000
)

type machineRunning struct {
	MachineID string `json:"machine_id"`
	Terminals int    `json:"terminals"`
	Tunnels   int    `json:"tunnels"`
	Previews  int    `json:"previews"`
}

type runningCounts struct {
	Items     []machineRunning `json:"items"`
	Truncated bool             `json:"truncated"`
}

type runningCountsEntry struct {
	done  chan struct{}
	at    time.Time
	value runningCounts
	err   error
}

type runningCountsCache struct {
	mu      sync.Mutex
	entries map[string]*runningCountsEntry
}

// get answers from a fresh entry or one computation shared by every caller of the key.
func (c *runningCountsCache) get(ctx context.Context, key string, now time.Time, compute func() (runningCounts, error)) (runningCounts, error) {
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]*runningCountsEntry{}
	}
	entry := c.entries[key]
	if entry == nil || (entry.at.Before(now.Add(-runningCountsTTL)) && isClosed(entry.done)) {
		if len(c.entries) >= runningCountsMaxKeys {
			for k, e := range c.entries {
				if isClosed(e.done) && e.at.Before(now.Add(-runningCountsTTL)) {
					delete(c.entries, k)
				}
			}
		}
		entry = &runningCountsEntry{done: make(chan struct{}), at: now}
		c.entries[key] = entry
		go func() {
			entry.value, entry.err = compute()
			if entry.err != nil {
				c.mu.Lock()
				delete(c.entries, key) // a failure is not cached
				c.mu.Unlock()
			}
			close(entry.done)
		}()
	}
	c.mu.Unlock()
	select {
	case <-entry.done:
		return entry.value, entry.err
	case <-ctx.Done():
		return runningCounts{}, ctx.Err()
	}
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// runningTerminals is the terminal collection's list path (*usermachines.Service).
type runningTerminals interface {
	TerminalSessionInventory(context.Context, string, usermachines.InventoryOptions) ([]usermachines.SharedTerminalSession, int, error)
}

func machineRunningCounts(machines runningTerminals, previews PreviewLeaseAPI, tunnels tunnelv1.API) http.HandlerFunc {
	cache := &runningCountsCache{}
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		for key := range r.URL.Query() {
			if key != "workspace" {
				writeError(w, r, http.StatusBadRequest, "invalid_inventory_query", "Running counts take only workspace.")
				return
			}
		}
		ctx := personalWorkspaceContext(r.Context())
		scope, _ := workspace.FromContext(ctx)
		request, _ := previewTunnelRequestContext(w, r)
		key := p.User.ID + "\x00" + scope.ID
		if p.Client != nil {
			key += "\x00" + p.Client.SessionID // a token's scopes decide what its lists show
		}
		// The shared computation outlives one caller's cancellation, bounded on its own.
		shared := context.WithoutCancel(ctx)
		counts, err := cache.get(r.Context(), key, time.Now(), func() (runningCounts, error) {
			computeCtx, cancel := context.WithTimeout(shared, 30*time.Second)
			defer cancel()
			return countRunning(computeCtx, machines, previews, tunnels, p.User.ID, request)
		})
		switch {
		case expectedHTTPErrorOnly(err, context.Canceled):
			return
		case expectedHTTPErrorOnly(err, previewtunnelapi.ErrForbidden):
			writeError(w, r, http.StatusForbidden, "forbidden", "These running counts need previews and tunnels read access.")
		case err != nil:
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Unable to count what the machines run.", err)
		default:
			writeJSON(w, http.StatusOK, SuccessResponse{Data: counts})
		}
	}
}

// countRunning walks the three collections through their own list paths and groups them by machine.
func countRunning(ctx context.Context, machines runningTerminals, previews PreviewLeaseAPI, tunnels tunnelv1.API, account string, request previewtunnelapi.RequestContext) (runningCounts, error) {
	byMachine := map[string]*machineRunning{}
	add := func(machine string, field func(*machineRunning) *int) {
		if machine == "" {
			return
		}
		entry := byMachine[machine]
		if entry == nil {
			entry = &machineRunning{MachineID: machine}
			byMachine[machine] = entry
		}
		*field(entry)++
	}
	var out runningCounts
	// Terminal sessions are the account's (GET /v1/terminal-sessions?state=running), stored rows only.
	for offset := 0; ; offset += 200 {
		sessions, total, err := machines.TerminalSessionInventory(ctx, account, usermachines.InventoryOptions{Limit: 200, Offset: offset, State: "running", WithoutLiveMetadata: true})
		if err != nil {
			return runningCounts{}, err
		}
		for _, session := range sessions {
			add(session.Target.MachineID, func(m *machineRunning) *int { return &m.Terminals })
		}
		if offset+len(sessions) >= total || len(sessions) == 0 {
			break
		}
		if offset+200 >= runningCountsMaxRows {
			out.Truncated = true
			break
		}
	}
	active := workspace.WithInventoryFilters(ctx, workspace.InventoryFilters{State: "active"})
	if tunnels != nil {
		cursor, rows := "", 0
		for {
			page, err := tunnels.ListTunnels(active, request, cursor, previewtunnelapi.MaximumPageLimit)
			if err != nil {
				return runningCounts{}, err
			}
			for _, tunnel := range page.Items {
				add(tunnel.CreatedByHostID, func(m *machineRunning) *int { return &m.Tunnels })
			}
			rows += len(page.Items)
			if page.NextCursor == "" {
				break
			}
			if rows >= runningCountsMaxRows {
				out.Truncated = true
				break
			}
			cursor = page.NextCursor
		}
	}
	if previews != nil {
		cursor, rows := "", 0
		for {
			page, err := previews.List(active, request, cursor, previewtunnelapi.MaximumPageLimit)
			if err != nil {
				return runningCounts{}, err
			}
			for _, preview := range page.Items {
				add(preview.OwnerMachineID, func(m *machineRunning) *int { return &m.Previews })
			}
			rows += len(page.Items)
			if page.NextCursor == "" {
				break
			}
			if rows >= runningCountsMaxRows {
				out.Truncated = true
				break
			}
			cursor = page.NextCursor
		}
	}
	out.Items = make([]machineRunning, 0, len(byMachine))
	for _, entry := range byMachine {
		out.Items = append(out.Items, *entry)
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].MachineID < out.Items[j].MachineID })
	return out, nil
}
