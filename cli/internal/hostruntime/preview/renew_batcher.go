package preview

import (
	"context"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
)

// A preview renews its lease every PreviewLeaseDefaultRenewInterval; N previews would be N requests
// per interval. The batcher collects the renewals that fall into one window and sends them as one
// request (F21-168: 1,000 previews cost ~10 requests per interval, not 1,000).
const (
	renewBatchWindow  = time.Second
	renewBatchTimeout = 30 * time.Second
)

type renewSender func(context.Context, []api.PreviewLeaseRenewal, string) ([]api.PreviewLeaseRenewalResult, error)

type renewBatcher struct {
	send   renewSender
	window time.Duration

	mu      sync.Mutex
	pending []*pendingRenewal
	timer   *time.Timer
}

type pendingRenewal struct {
	item api.PreviewLeaseRenewal
	done chan api.PreviewLeaseRenewalResult // buffered: a flush never blocks on a caller that left
}

func newRenewBatcher(send renewSender) *renewBatcher {
	return &renewBatcher{send: send, window: renewBatchWindow}
}

// renew queues one lease and waits for its own result; ctx abandons the wait, not the request: the
// item keeps its idempotency key, so the caller's retry is safe.
func (b *renewBatcher) renew(ctx context.Context, item api.PreviewLeaseRenewal) (api.PreviewLease, error) {
	p := &pendingRenewal{item: item, done: make(chan api.PreviewLeaseRenewalResult, 1)}
	b.mu.Lock()
	b.pending = append(b.pending, p)
	if len(b.pending) >= api.MaxPreviewLeaseRenewals {
		b.flushLocked()
	} else if b.timer == nil {
		b.timer = time.AfterFunc(b.window, func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.flushLocked()
		})
	}
	b.mu.Unlock()
	select {
	case result := <-p.done:
		return result.Lease, result.Err
	case <-ctx.Done():
		return api.PreviewLease{}, ctx.Err()
	}
}

// flushLocked sends the queued renewals, at most one per lease (the server refuses a batch that
// names a lease twice; a retry of an abandoned renewal waits for the next window).
func (b *renewBatcher) flushLocked() {
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	var batch, later []*pendingRenewal
	seen := map[string]bool{}
	for _, p := range b.pending {
		if seen[p.item.Lease.ID] || len(batch) == api.MaxPreviewLeaseRenewals {
			later = append(later, p)
			continue
		}
		seen[p.item.Lease.ID] = true
		batch = append(batch, p)
	}
	b.pending = later
	if len(later) > 0 {
		b.timer = time.AfterFunc(b.window, func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.flushLocked()
		})
	}
	if len(batch) > 0 {
		go b.run(batch)
	}
}

func (b *renewBatcher) run(batch []*pendingRenewal) {
	fail := func(err error) {
		for _, p := range batch {
			p.done <- api.PreviewLeaseRenewalResult{Err: err}
		}
	}
	key, err := newSessionIdempotencyKey(nil)
	if err != nil {
		fail(err)
		return
	}
	items := make([]api.PreviewLeaseRenewal, len(batch))
	for i, p := range batch {
		items[i] = p.item
	}
	ctx, cancel := context.WithTimeout(context.Background(), renewBatchTimeout)
	defer cancel()
	results, err := b.send(ctx, items, key)
	if err != nil {
		fail(err)
		return
	}
	for i, p := range batch {
		p.done <- results[i]
	}
}
