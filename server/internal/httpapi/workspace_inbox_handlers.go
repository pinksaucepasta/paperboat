package httpapi

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/inboxdispatch"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// inboxMachineSource is the machines whose Inbox an actor may manage (*usermachines.Service).
type inboxMachineSource interface {
	InboxMachines(ctx context.Context, userID, after, only string, limit int) ([]usermachines.InboxMachine, bool, error)
}

type inboxListMachine struct {
	ID    string `json:"id"`
	Alias string `json:"alias"`
}

type inboxListItem struct {
	inboxdispatch.Item
	Machine inboxListMachine `json:"machine"`
}

type inboxUnavailable struct {
	MachineID string `json:"machine_id"`
	Alias     string `json:"alias"`
	Reason    string `json:"reason"`
}

// inboxBatch is one batch of machines' Inboxes as they answered: the files merged newest first, and why the
// others could not be read.
type inboxBatch struct {
	items       []inboxListItem
	checked     int
	files       int // the checked machines' own counts (a listing holds at most MaxListItems)
	truncated   []string
	unavailable []inboxUnavailable
	next        string
	expires     time.Time
}

// inboxBatchCache reuses a batch for the policy's CacheFor, so paging through it and refining q does not ask
// the machines again; it holds at most inboxBatchCacheSize batches.
type inboxBatchCache struct {
	mu      sync.Mutex
	batches map[string]*inboxBatch
}

const inboxBatchCacheSize = 1024

func (c *inboxBatchCache) get(key string, now time.Time) *inboxBatch {
	c.mu.Lock()
	defer c.mu.Unlock()
	if batch := c.batches[key]; batch != nil && now.Before(batch.expires) {
		return batch
	}
	return nil
}

func (c *inboxBatchCache) put(key string, batch *inboxBatch, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.batches == nil {
		c.batches = map[string]*inboxBatch{}
	}
	if len(c.batches) >= inboxBatchCacheSize {
		for k, b := range c.batches {
			if !now.Before(b.expires) {
				delete(c.batches, k)
			}
		}
		for k := range c.batches {
			if len(c.batches) < inboxBatchCacheSize {
				break
			}
			delete(c.batches, k)
		}
	}
	c.batches[key] = batch
}

// workspaceInboxList is GET /v1/inbox: the received files of the selected workspace's machines, read live from
// the machines (the server stores no file names), at most policy.Machines machines per request in machine id
// order, with a machine cursor for the next batch. Totals are of the machines checked so far, never of the whole
// workspace.
func workspaceInboxList(machines inboxMachineSource, inbox InboxAPI, policy config.InboxListPolicy, now func() time.Time) http.HandlerFunc {
	cache := &inboxBatchCache{}
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		bad := func() {
			writeError(w, r, http.StatusBadRequest, "invalid_inventory_query", "Use machine_cursor from next_machine_cursor, machine_id and q (a file name, at most 255 bytes).")
		}
		values := r.URL.Query()
		for key, value := range values {
			if len(value) > 1 || (key != "machine_cursor" && key != "machine_id" && key != "q" && key != "workspace") {
				bad()
				return
			}
		}
		q := strings.ToLower(values.Get("q"))
		only := values.Get("machine_id")
		if !utf8.ValidString(q) || len(q) > 255 || strings.ContainsRune(q, '\x00') || len(only) > 128 || strings.ContainsAny(only, " \t\r\n\x00") {
			bad()
			return
		}
		after := ""
		if cursor := values.Get("machine_cursor"); cursor != "" {
			raw, err := base64.RawURLEncoding.DecodeString(cursor)
			if err != nil || len(raw) == 0 || len(raw) > 128 || !utf8.Valid(raw) {
				bad()
				return
			}
			after = string(raw)
		}
		ctx := personalWorkspaceContext(r.Context())
		scope, _ := workspace.FromContext(ctx)
		key := strings.Join([]string{principal.User.ID, scope.ID, after, only}, "\x00")
		batch := cache.get(key, now())
		if batch == nil {
			var err error
			if batch, err = readInboxBatch(ctx, machines, inbox, policy, principal.User.ID, scope.TeamID, after, only); err != nil {
				writeError(w, r, http.StatusInternalServerError, "internal_error", "Unable to list the machines' Inboxes.", err)
				return
			}
			batch.expires = now().Add(policy.CacheFor)
			cache.put(key, batch, now())
		}
		items := make([]inboxListItem, 0, len(batch.items))
		for _, item := range batch.items {
			if q == "" || strings.Contains(strings.ToLower(item.Name), q) {
				items = append(items, item)
			}
		}
		data := map[string]any{"items": items, "machines_checked": batch.checked, "files_on_checked_machines": batch.files,
			"truncated": append([]string{}, batch.truncated...), "unavailable": append([]inboxUnavailable{}, batch.unavailable...)}
		if batch.next != "" {
			data["next_machine_cursor"] = base64.RawURLEncoding.EncodeToString([]byte(batch.next))
		}
		noStore(w)
		writeJSON(w, http.StatusOK, SuccessResponse{Data: data})
	}
}

// inboxListConcurrency is how many machines one request asks at once.
const inboxListConcurrency = 8

func readInboxBatch(ctx context.Context, machines inboxMachineSource, inbox InboxAPI, policy config.InboxListPolicy, actor, team, after, only string) (*inboxBatch, error) {
	found, more, err := machines.InboxMachines(ctx, actor, after, only, policy.Machines)
	if err != nil {
		return nil, err
	}
	batch := &inboxBatch{checked: len(found), items: []inboxListItem{}}
	if more && len(found) > 0 {
		batch.next = found[len(found)-1].ID
	}
	budget, cancel := context.WithTimeout(ctx, policy.Budget)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	gate := make(chan struct{}, inboxListConcurrency)
	for _, machine := range found {
		if !machine.Receives {
			batch.unavailable = append(batch.unavailable, inboxUnavailable{machine.ID, machine.Alias, "inbox_unavailable"})
			continue
		}
		if !machine.Online || machine.State != "online" {
			batch.unavailable = append(batch.unavailable, inboxUnavailable{machine.ID, machine.Alias, "machine_offline"})
			continue
		}
		wg.Add(1)
		gate <- struct{}{}
		go func() {
			defer func() { <-gate; wg.Done() }()
			request := inboxdispatch.Request{ActorID: actor, MachineID: machine.ID, OperationID: "inbox-" + uuid.NewString(),
				RequestID: requestIDFromContext(ctx), CorrelationID: supportReference(ctx)}
			if team != "" {
				request.TeamID = sql.NullString{String: team, Valid: true}
			}
			listing, err := inbox.List(budget, request)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				batch.unavailable = append(batch.unavailable, inboxUnavailable{machine.ID, machine.Alias, inboxUnavailableReason(err, budget)})
				return
			}
			batch.files += listing.Total
			if listing.Total > len(listing.Items) {
				batch.truncated = append(batch.truncated, machine.ID)
			}
			for _, item := range listing.Items {
				batch.items = append(batch.items, inboxListItem{Item: item, Machine: inboxListMachine{ID: machine.ID, Alias: machine.Alias}})
			}
		}()
	}
	wg.Wait()
	sort.Slice(batch.items, func(i, j int) bool {
		a, b := batch.items[i], batch.items[j]
		if !a.ReceivedAt.Equal(b.ReceivedAt) {
			return a.ReceivedAt.After(b.ReceivedAt)
		}
		if a.Machine.ID != b.Machine.ID {
			return a.Machine.ID < b.Machine.ID
		}
		return a.ID < b.ID
	})
	sort.Slice(batch.unavailable, func(i, j int) bool { return batch.unavailable[i].MachineID < batch.unavailable[j].MachineID })
	sort.Strings(batch.truncated)
	return batch, nil
}

// inboxUnavailableReason is the per-machine form of writeInboxDispatchError's codes.
func inboxUnavailableReason(err error, budget context.Context) string {
	switch {
	case errors.Is(err, inboxdispatch.ErrMachineOffline):
		return "machine_offline"
	case errors.Is(err, inboxdispatch.ErrMachineOutdated):
		return "machine_update_required"
	case errors.Is(err, inboxdispatch.ErrCapabilityDisabled):
		return "inbox_unavailable"
	case budget.Err() != nil:
		return "timeout"
	default:
		return "machine_unreachable"
	}
}
