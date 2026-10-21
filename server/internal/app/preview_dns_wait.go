package app

import (
	"context"
	"log/slog"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	previewAddressWaitBudget = 8 * time.Second
	previewAddressWaitPoll   = 300 * time.Millisecond
	// A provider's anycast servers answer from several points of presence while a new record spreads; the
	// name counts as published only after the answer was right this many times in a row.
	previewAddressStableAnswers = 3
)

// waitForAddresses polls lookup until it returns exactly want previewAddressStableAnswers times in a row, or the
// budget ends. It reports whether the answers became stable.
func waitForAddresses(ctx context.Context, lookup func(context.Context) ([]string, error), want []string, poll, budget time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	want = slices.Clone(want)
	slices.Sort(want)
	stable := 0
	for {
		got, err := lookup(ctx)
		slices.Sort(got)
		if err == nil && slices.Equal(got, want) {
			if stable++; stable >= previewAddressStableAnswers {
				return true
			}
		} else {
			stable = 0
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(poll):
		}
	}
}

// AwaitPreviewAddress returns once the zone's authoritative servers answer with the addresses published for the
// preview, so the URL is announced as ready only when a resolver that looks it up gets the edge that holds the
// route and not the shared wildcard's. It never fails the preview: when the budget ends the preview proceeds.
func (p *previewAddressPublisher) AwaitPreviewAddress(ctx context.Context, previewID string) {
	var host string
	var desired []string
	err := p.database.Pool().QueryRow(ctx, `SELECT pub.hostname, ARRAY(SELECT jsonb_array_elements_text(pub.desired_addresses))
FROM paperboat.preview_leases l JOIN paperboat.edge_dns_publications pub ON pub.hostname=substring(l.endpoint from 9)
WHERE l.id=$1 AND l.terminal_state='active'`, previewID).Scan(&host, &desired)
	if err == pgx.ErrNoRows || err == nil && len(desired) == 0 {
		return
	}
	if err != nil {
		slog.WarnContext(ctx, "preview address wait skipped", "component", "paperboat-server", "operation", "http_previews", "stage", "dns_wait", "error_type", "database")
		return
	}
	servers := p.authoritativeServers(ctx, host)
	if len(servers) == 0 {
		return
	}
	lookup := func(ctx context.Context) ([]string, error) {
		resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "udp", servers[0])
		}}
		return resolver.LookupHost(ctx, host)
	}
	if !waitForAddresses(ctx, lookup, desired, previewAddressWaitPoll, previewAddressWaitBudget) {
		slog.WarnContext(ctx, "preview address not yet visible at the provider's servers; continuing", "component", "paperboat-server", "operation", "http_previews", "stage", "dns_wait")
	}
}

// authoritativeServers returns the host:port of one name server of the preview's parent domain.
func (p *previewAddressPublisher) authoritativeServers(ctx context.Context, host string) []string {
	parent := host[strings.Index(host, ".")+1:]
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	servers, err := net.DefaultResolver.LookupNS(lookupCtx, parent)
	if err != nil || len(servers) == 0 {
		return nil
	}
	addresses, err := net.DefaultResolver.LookupHost(lookupCtx, strings.TrimSuffix(servers[0].Host, "."))
	if err != nil || len(addresses) == 0 {
		return nil
	}
	return []string{net.JoinHostPort(addresses[0], "53")}
}
