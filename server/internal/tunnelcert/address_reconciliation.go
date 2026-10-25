package tunnelcert

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

const maxDesiredAddressRecords = 4

type AddressRecordProvider interface {
	ListAddressRecords(context.Context, string, string) ([]AddressRecord, error)
	CreateAddressRecord(context.Context, AddressRecord, bool) (AddressRecord, error)
	DeleteAddressRecord(context.Context, AddressRecord) error
}

type AddressResolver interface {
	LookupHost(context.Context, string) ([]string, error)
}

type DesiredAddressPublication struct {
	Owner                    string
	Hostname                 string
	ResourceGeneration       uint64
	ReadinessVersion         string
	ObservedAt               time.Time
	ValidUntil               time.Time
	Addresses                []string
	IPv6ReachabilityVerified bool
}

type AddressReconciler struct {
	database *db.DB
	provider AddressRecordProvider
	resolver AddressResolver
	now      func() time.Time
}

// The budget statements take their numbers from the two constants in cloudflare_addresses.go.
var (
	acquireBudgetSQL = fmt.Sprintf(`INSERT INTO edge_dns_provider_budgets(zone_id,tokens,refilled_at,blocked_until)
VALUES($1,%[1]d-1,statement_timestamp(),statement_timestamp())
ON CONFLICT(zone_id) DO UPDATE SET
tokens=LEAST(%[1]d,edge_dns_provider_budgets.tokens+EXTRACT(EPOCH FROM (statement_timestamp()-edge_dns_provider_budgets.refilled_at))/%[2]g)-1,
refilled_at=statement_timestamp()
WHERE edge_dns_provider_budgets.blocked_until<=statement_timestamp()
  AND LEAST(%[1]d,edge_dns_provider_budgets.tokens+EXTRACT(EPOCH FROM (statement_timestamp()-edge_dns_provider_budgets.refilled_at))/%[2]g)>=1
RETURNING tokens`, cloudflareAddressBurst, cloudflareAddressRefillSeconds)
	reserveBudgetSQL = fmt.Sprintf(`INSERT INTO edge_dns_provider_budgets(zone_id,tokens,refilled_at,blocked_until)
VALUES($1,%[1]d-$2,statement_timestamp(),statement_timestamp())
ON CONFLICT(zone_id) DO UPDATE SET
 tokens=LEAST(%[1]d,edge_dns_provider_budgets.tokens+EXTRACT(EPOCH FROM(statement_timestamp()-edge_dns_provider_budgets.refilled_at))/%[2]g)-$2,
 refilled_at=statement_timestamp()
WHERE edge_dns_provider_budgets.blocked_until<=statement_timestamp()
 AND LEAST(%[1]d,edge_dns_provider_budgets.tokens+EXTRACT(EPOCH FROM(statement_timestamp()-edge_dns_provider_budgets.refilled_at))/%[2]g)>=$2
RETURNING tokens`, cloudflareAddressBurst, cloudflareAddressRefillSeconds)
	budgetWaitSQL   = fmt.Sprintf(`SELECT GREATEST(0,EXTRACT(EPOCH FROM (blocked_until-statement_timestamp())),(1-tokens)*%g-EXTRACT(EPOCH FROM (statement_timestamp()-refilled_at))) FROM edge_dns_provider_budgets WHERE zone_id=$1`, cloudflareAddressRefillSeconds)
	refundBudgetSQL = fmt.Sprintf(`UPDATE edge_dns_provider_budgets SET tokens=LEAST(%d,tokens+$2) WHERE zone_id=$1`, cloudflareAddressBurst)
)

type databaseCloudflareAddressAdmission struct {
	database *db.DB
	zoneID   string
}

// NewDatabaseCloudflareAddressAdmission creates the shared per-zone provider
// budget used by every server replica configured with the same database.
func NewDatabaseCloudflareAddressAdmission(database *db.DB, zoneID string) (CloudflareAddressAdmission, error) {
	if database == nil || database.Direct() == nil || !validMetadata(zoneID, 128) {
		return nil, fmt.Errorf("%w: Cloudflare address admission database and zone are required", ErrInvalid)
	}
	return &databaseCloudflareAddressAdmission{database: database, zoneID: zoneID}, nil
}

func (a *databaseCloudflareAddressAdmission) Acquire(ctx context.Context) (func(), error) {
	if reservation, ok := ctx.Value(addressReservationKey{}).(*addressReservation); ok && reservation.owner == a {
		return reservation.acquire(ctx)
	}
	for {
		conn, err := a.database.Direct().Acquire(ctx)
		if err != nil {
			return nil, err
		}
		slot := -1
		for candidate := 0; candidate < 2; candidate++ {
			var locked bool
			if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,173)+$2)`, a.zoneID, candidate).Scan(&locked); err != nil {
				conn.Release()
				return nil, err
			}
			if locked {
				slot = candidate
				break
			}
		}
		if slot < 0 {
			conn.Release()
			if err := waitContext(ctx, 25*time.Millisecond); err != nil {
				return nil, err
			}
			continue
		}
		release := databaseAdmissionRelease(conn, a.zoneID, slot)
		var remaining float64
		err = conn.QueryRow(ctx, acquireBudgetSQL, a.zoneID).Scan(&remaining)
		if err == nil {
			return release, nil
		}
		release()
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		wait, err := a.budgetWait(ctx)
		if err != nil {
			return nil, err
		}
		if err := waitContext(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// budgetWait is how long to wait before the zone's provider budget can admit a call (at least 25 ms). It holds no
// session state, so it runs on the pooled handle.
func (a *databaseCloudflareAddressAdmission) budgetWait(ctx context.Context) (time.Duration, error) {
	var waitSeconds float64
	if err := a.database.Pool().QueryRow(ctx, budgetWaitSQL, a.zoneID).Scan(&waitSeconds); err != nil {
		return 0, err
	}
	return max(time.Duration(waitSeconds*float64(time.Second)), 25*time.Millisecond), nil
}

func (a *databaseCloudflareAddressAdmission) Reserve(ctx context.Context, count int) (context.Context, func(), error) {
	if count < 1 || count > cloudflareAddressBurst {
		return nil, nil, ErrInvalid
	}
	conn, err := a.database.Direct().Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	slot := -1
	for candidate := 0; candidate < 2; candidate++ {
		var locked bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,173)+$2)`, a.zoneID, candidate).Scan(&locked); err != nil {
			conn.Release()
			return nil, nil, err
		}
		if locked {
			slot = candidate
			break
		}
	}
	if slot < 0 {
		conn.Release()
		return nil, nil, addressError(AddressPublicationPending, "provider concurrency unavailable")
	}
	release := databaseAdmissionRelease(conn, a.zoneID, slot)
	var remaining float64
	err = conn.QueryRow(ctx, reserveBudgetSQL, a.zoneID, count).Scan(&remaining)
	if err != nil {
		release()
		if errors.Is(err, pgx.ErrNoRows) {
			err = addressError(AddressPublicationPending, "provider step budget unavailable")
		}
		return nil, nil, err
	}
	reservation := &addressReservation{owner: a, remaining: count, concurrent: make(chan struct{}, 1)}
	reservation.check = func(ctx context.Context) error {
		var blocked bool
		err := conn.QueryRow(ctx, `SELECT blocked_until>statement_timestamp() FROM edge_dns_provider_budgets WHERE zone_id=$1`, a.zoneID).Scan(&blocked)
		if err != nil {
			return err
		}
		if blocked {
			return addressError(AddressPublicationPending, "provider retry after active")
		}
		return nil
	}
	reservation.refund = func(unused int) {
		if unused > 0 {
			refundCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, _ = conn.Exec(refundCtx, refundBudgetSQL, a.zoneID, unused)
		}
		release()
	}
	return context.WithValue(ctx, addressReservationKey{}, reservation), reservation.close, nil
}

func databaseAdmissionRelease(conn *pgxpool.Conn, zoneID string, slot int) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var unlocked bool
			if err := conn.QueryRow(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,173)+$2)`, zoneID, slot).Scan(&unlocked); err != nil || !unlocked {
				_ = conn.Conn().Close(ctx)
			}
			conn.Release()
		})
	}
}

func (a *databaseCloudflareAddressAdmission) Block(_ context.Context, retryAfter string) error {
	delay := providerBlockDelay(retryAfter, time.Now().UTC())
	if delay <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := a.database.Pool().Exec(ctx, `INSERT INTO edge_dns_provider_budgets(zone_id,tokens,refilled_at,blocked_until)
VALUES($1,5,statement_timestamp(),statement_timestamp()+$2::interval) ON CONFLICT(zone_id) DO UPDATE SET blocked_until=GREATEST(edge_dns_provider_budgets.blocked_until,EXCLUDED.blocked_until)`, a.zoneID, delay.String())
	return err
}

func NewAddressReconciler(database *db.DB, provider AddressRecordProvider, resolver AddressResolver) (*AddressReconciler, error) {
	if database == nil || database.Direct() == nil || provider == nil || resolver == nil {
		return nil, fmt.Errorf("%w: address reconciler dependencies are required", ErrInvalid)
	}
	return &AddressReconciler{database: database, provider: provider, resolver: resolver, now: time.Now}, nil
}

func (r *AddressReconciler) OwnedHostnames(ctx context.Context, owner string) ([]string, error) {
	if r == nil || r.database == nil || !validMetadata(owner, 128) {
		return nil, fmt.Errorf("%w: publication owner is invalid", ErrInvalid)
	}
	hostnames, err := r.database.Queries().ListEdgeDNSPublicationHostnamesByOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	if len(hostnames) > 128 {
		return nil, addressError(AddressPublicationConflict, "publication hostname ownership exceeds bounds")
	}
	return hostnames, nil
}

// Reconcile serializes one hostname from durable desired state through
// provider mutation and resolver verification. Provider errors are returned
// unchanged so pending publication is never mistaken for readiness.
func (r *AddressReconciler) Reconcile(ctx context.Context, desired DesiredAddressPublication) error {
	return r.reconcile(ctx, desired, false)
}

// readbackRequiredDetail marks the pass that changed provider records and stopped to read them back.
const (
	readbackRequiredDetail = "provider changes require independent readback"
	notConvergedDetail     = "provider records have not converged"
	resolverWaitDetail     = "authoritative address results have not converged"
)

const maxPublishPasses = 8

// WaitingForDNS reports whether err only means that DNS has not caught up with records the provider
// holds yet (a normal state for about one TTL after every change), as opposed to a failure.
func WaitingForDNS(err error) bool {
	var typed *AddressPublicationError
	if !errors.As(err, &typed) || typed.Kind != AddressPublicationPending {
		return false
	}
	return typed.Detail == readbackRequiredDetail || typed.Detail == notConvergedDetail || typed.Detail == resolverWaitDetail
}

// Publish writes the desired records to the provider and returns once the provider holds exactly
// them, without waiting for resolver verification: a caller that must not wait on DNS (a new
// preview) proceeds on the written record, and the periodic Reconcile verifies it afterwards. An
// unchanged publication that is already written returns without touching the provider.
func (r *AddressReconciler) Publish(ctx context.Context, desired DesiredAddressPublication) error {
	for pass := 0; pass < maxPublishPasses; pass++ {
		err := r.reconcile(ctx, desired, true)
		var publicationError *AddressPublicationError
		if err == nil {
			return nil
		}
		// Each pass changes at most one record; both of these results mean "changed one, go on".
		if !errors.As(err, &publicationError) || publicationError.Kind != AddressPublicationPending ||
			publicationError.Detail != readbackRequiredDetail && publicationError.Detail != notConvergedDetail {
			return err
		}
	}
	return addressError(AddressPublicationPending, "provider records did not converge")
}

func (r *AddressReconciler) reconcile(ctx context.Context, desired DesiredAddressPublication, publishOnly bool) error {
	if r == nil || r.database == nil || r.provider == nil || r.resolver == nil {
		return fmt.Errorf("%w: address reconciler is unavailable", ErrInvalid)
	}
	host, addresses, err := validateDesiredAddressPublication(desired)
	if err != nil {
		return err
	}
	desired.Hostname, desired.Addresses = host, addresses

	conn, err := r.database.Direct().Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err = conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 172))`, host); err != nil {
		return err
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var unlocked bool
		if err := conn.QueryRow(unlockCtx, `SELECT pg_advisory_unlock(hashtextextended($1, 172))`, host).Scan(&unlocked); err != nil || !unlocked {
			_ = conn.Conn().Close(unlockCtx)
		}
	}()

	generation, err := r.persistDesired(ctx, conn.Conn(), desired)
	if err != nil {
		return err
	}
	// Reuse unchanged verified publication for one DNS TTL. Fresh resource
	// authority is persisted above; any changed tuple clears verified state.
	current, readErr := dbsqlc.New(conn.Conn()).GetEdgeDNSPublication(ctx, host)
	if readErr != nil {
		return readErr
	}
	if current.State == "verified" && current.VerifiedAt.Valid && r.now().Sub(current.VerifiedAt.Time) < 60*time.Second {
		return nil
	}
	if publishOnly && (current.State == "submitted" || current.State == "verified") && len(addresses) != 0 {
		return nil
	}
	if provider, ok := r.provider.(interface {
		ReserveAddressStep(context.Context) (context.Context, func(), error)
	}); ok {
		reserved, release, reserveErr := provider.ReserveAddressStep(ctx)
		if reserveErr != nil {
			return reserveErr
		}
		defer release()
		ctx = reserved
	}
	records, err := r.provider.ListAddressRecords(ctx, host, desired.Owner)
	if err != nil {
		return r.recordFailure(ctx, conn.Conn(), desired, generation, nil, err)
	}
	wanted := stringSet(addresses)
	mutated := false
	retained := make(map[string]bool, len(records))
	kept := records[:0]
	for _, record := range records {
		if record.Generation > generation {
			err := addressError(AddressPublicationConflict, "provider record has a future publication generation")
			return r.recordFailure(ctx, conn.Conn(), desired, generation, records, err)
		}
		_, wantedAddress := wanted[record.Address]
		if wantedAddress && !retained[record.Address] {
			kept = append(kept, record)
			retained[record.Address] = true
			continue
		}
		if mutated {
			kept = append(kept, record)
			continue
		}
		writeCtx, cancel, deadlineErr := publicationWriteContext(ctx, desired)
		if deadlineErr != nil {
			return deadlineErr
		}
		deleteErr := r.provider.DeleteAddressRecord(writeCtx, record)
		cancel()
		if deleteErr != nil {
			return r.recordFailure(ctx, conn.Conn(), desired, generation, records, deleteErr)
		}
		mutated = true
	}
	records = kept
	present := make(map[string]bool, len(records))
	for _, record := range records {
		present[record.Address] = true
	}
	for _, address := range addresses {
		if mutated {
			break
		}
		if present[address] {
			continue
		}
		writeCtx, cancel, deadlineErr := publicationWriteContext(ctx, desired)
		if deadlineErr != nil {
			return deadlineErr
		}
		created, createErr := r.provider.CreateAddressRecord(writeCtx, AddressRecord{Hostname: host, Address: address, Owner: desired.Owner, Generation: generation}, desired.IPv6ReachabilityVerified)
		cancel()
		if createErr != nil {
			return r.recordFailure(ctx, conn.Conn(), desired, generation, records, createErr)
		}
		records = append(records, created)
		mutated = true
	}
	if !addressRecordsMatch(records, desired.Owner, generation, wanted) {
		err = addressError(AddressPublicationPending, notConvergedDetail)
		return r.recordFailure(ctx, conn.Conn(), desired, generation, records, err)
	}
	if err := r.markState(ctx, conn.Conn(), desired, generation, records, "submitted", ""); err != nil {
		return err
	}
	if publishOnly {
		// The provider confirmed every write above; the periodic reconcile reads them back.
		return nil
	}
	if mutated {
		return addressError(AddressPublicationPending, readbackRequiredDetail)
	}
	if err := r.verifyAddressResolution(ctx, host, wanted); err != nil {
		return err
	}
	return r.markState(ctx, conn.Conn(), desired, generation, records, "verified", "")
}

func (r *AddressReconciler) verifyAddressResolution(ctx context.Context, host string, wanted map[string]struct{}) error {
	resolved, lookupErr := r.resolver.LookupHost(ctx, addressLookupHostname(host))
	if lookupErr != nil && !(len(wanted) == 0 && isDNSNotFound(lookupErr)) {
		return addressErrorWithCauses(AddressPublicationPending, "authoritative address lookup failed", 0, lookupErr)
	}
	if !resolvedAddressesMatch(resolved, wanted) {
		// Provider readback above proved that the exact A/AAAA records are gone.
		// A parent wildcard can still answer this name; independently compare a
		// fresh sibling before accepting that inherited response as withdrawal.
		_, parent, exact := strings.Cut(host, ".")
		if len(wanted) != 0 || !exact || strings.HasPrefix(host, "*.") {
			return addressError(AddressPublicationPending, resolverWaitDetail)
		}
		inherited, err := r.resolver.LookupHost(ctx, addressLookupHostname("*."+parent))
		if err != nil {
			return addressErrorWithCauses(AddressPublicationPending, "parent wildcard lookup failed", 0, err)
		}
		if len(inherited) == 0 || !resolvedAddressesMatch(resolved, stringSet(inherited)) {
			return addressError(AddressPublicationPending, "withdrawn address does not match the parent wildcard")
		}
	}
	return nil
}

func (r *AddressReconciler) persistDesired(ctx context.Context, conn *pgx.Conn, desired DesiredAddressPublication) (uint64, error) {
	encoded, _ := json.Marshal(desired.Addresses)
	row, err := dbsqlc.New(conn).UpsertEdgeDNSPublicationDesired(ctx, dbsqlc.UpsertEdgeDNSPublicationDesiredParams{
		Hostname: desired.Hostname, OwnerID: desired.Owner, ResourceGeneration: int64(desired.ResourceGeneration),
		ReadinessVersion: desired.ReadinessVersion, ObservedAt: desired.ObservedAt.UTC(),
		ValidUntil:       sql.NullTime{Time: desired.ValidUntil.UTC(), Valid: !desired.ValidUntil.IsZero()},
		DesiredAddresses: encoded, Now: r.now().UTC(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, addressError(AddressPublicationConflict, "hostname is owned by another resource or newer generation")
	}
	if err != nil {
		return 0, err
	}
	return uint64(row.PublicationGeneration), nil
}

func (r *AddressReconciler) recordFailure(ctx context.Context, conn *pgx.Conn, desired DesiredAddressPublication, generation uint64, records []AddressRecord, cause error) error {
	kind := "provider_failure"
	var publicationError *AddressPublicationError
	if errors.As(cause, &publicationError) && publicationError != nil {
		kind = string(publicationError.Kind)
	} else {
		publicationError = nil
	}
	if err := r.markState(ctx, conn, desired, generation, records, "failed", kind); err != nil {
		publicationKind := AddressPublicationPending
		if publicationError != nil {
			publicationKind = publicationError.Kind
		}
		return addressErrorWithCauses(publicationKind, "provider failure could not be recorded", 0, cause, err)
	}
	return cause
}

func (r *AddressReconciler) markState(ctx context.Context, conn *pgx.Conn, desired DesiredAddressPublication, generation uint64, records []AddressRecord, state, failure string) error {
	if records == nil {
		records = []AddressRecord{}
	}
	encoded, _ := json.Marshal(records)
	now := r.now().UTC()
	queries := dbsqlc.New(conn)
	var rows int64
	var err error
	switch state {
	case "submitted":
		rows, err = queries.MarkEdgeDNSPublicationSubmitted(ctx, dbsqlc.MarkEdgeDNSPublicationSubmittedParams{ProviderRecords: encoded, Now: now, Hostname: desired.Hostname, OwnerID: desired.Owner, PublicationGeneration: int64(generation)})
	case "verified":
		rows, err = queries.MarkEdgeDNSPublicationVerified(ctx, dbsqlc.MarkEdgeDNSPublicationVerifiedParams{ProviderRecords: encoded, Now: sql.NullTime{Time: now, Valid: true}, Hostname: desired.Hostname, OwnerID: desired.Owner, PublicationGeneration: int64(generation)})
	case "failed":
		rows, err = queries.MarkEdgeDNSPublicationFailed(ctx, dbsqlc.MarkEdgeDNSPublicationFailedParams{ProviderRecords: encoded, FailureCode: sql.NullString{String: failure, Valid: failure != ""}, Now: now, Hostname: desired.Hostname, OwnerID: desired.Owner, PublicationGeneration: int64(generation)})
	default:
		return fmt.Errorf("%w: publication state is invalid", ErrInvalid)
	}
	if err != nil {
		return err
	}
	if rows != 1 {
		return addressError(AddressPublicationConflict, "publication generation changed")
	}
	return nil
}

func validateDesiredAddressPublication(desired DesiredAddressPublication) (string, []string, error) {
	host, _, err := normalizeHostname(desired.Hostname)
	if err != nil || !validMetadata(desired.Owner, 128) || desired.ResourceGeneration == 0 || desired.ResourceGeneration > maxPlatformGeneration || desired.ObservedAt.IsZero() || len(desired.Addresses) > maxDesiredAddressRecords {
		return "", nil, fmt.Errorf("%w: desired address publication is invalid", ErrInvalid)
	}
	set := make(map[string]struct{}, len(desired.Addresses))
	addresses := make([]string, 0, len(desired.Addresses))
	for _, raw := range desired.Addresses {
		ip, parseErr := netip.ParseAddr(raw)
		if parseErr != nil || !publicAddress(ip) || ip.Is6() && !desired.IPv6ReachabilityVerified {
			return "", nil, fmt.Errorf("%w: desired address is invalid", ErrInvalid)
		}
		canonical := ip.Unmap().String()
		if _, exists := set[canonical]; exists {
			continue
		}
		set[canonical] = struct{}{}
		addresses = append(addresses, canonical)
	}
	sort.Strings(addresses)
	if len(addresses) > 0 && (!validMetadata(desired.ReadinessVersion, 256) || desired.ValidUntil.IsZero() || !desired.ValidUntil.After(time.Now())) {
		return "", nil, fmt.Errorf("%w: ready address authority is invalid or expired", ErrInvalid)
	}
	return host, addresses, nil
}

func publicationWriteContext(parent context.Context, desired DesiredAddressPublication) (context.Context, context.CancelFunc, error) {
	if len(desired.Addresses) == 0 {
		ctx, cancel := context.WithCancel(parent)
		return ctx, cancel, nil
	}
	if !desired.ValidUntil.After(time.Now()) {
		return nil, nil, addressError(AddressPublicationPending, "ready address authority expired before provider write")
	}
	ctx, cancel := context.WithDeadline(parent, desired.ValidUntil)
	return ctx, cancel, nil
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func addressRecordsMatch(records []AddressRecord, owner string, generation uint64, wanted map[string]struct{}) bool {
	if len(records) != len(wanted) {
		return false
	}
	seen := make(map[string]struct{}, len(records))
	for _, record := range records {
		if record.Owner != owner || record.Generation == 0 || record.Generation > generation {
			return false
		}
		if _, ok := wanted[record.Address]; !ok {
			return false
		}
		seen[record.Address] = struct{}{}
	}
	return len(seen) == len(wanted)
}

func addressLookupHostname(host string) string {
	if !strings.HasPrefix(host, "*.") {
		return host
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return "pb-verify." + strings.TrimPrefix(host, "*.")
	}
	return "pb-" + hex.EncodeToString(random) + "." + strings.TrimPrefix(host, "*.")
}

func isDNSNotFound(err error) bool {
	dnsError, ok := err.(*net.DNSError)
	return ok && dnsError != nil && dnsError.IsNotFound
}

func resolvedAddressesMatch(values []string, wanted map[string]struct{}) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		ip, err := netip.ParseAddr(value)
		if err != nil {
			return false
		}
		address := ip.Unmap().String()
		if _, ok := wanted[address]; !ok {
			return false
		}
		seen[address] = struct{}{}
	}
	return len(seen) == len(wanted)
}
