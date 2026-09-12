package inspector

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Record is one sanitized daemon-local capture. Raw bytes are never part of
// this shape; use GetRaw with a replay credential for the separate raw store.
type Record struct {
	ID                 string
	ResourceID         string
	ResourceGeneration uint64
	RouteGeneration    uint64
	TargetGeneration   uint64
	Method             string
	URL                string
	URLTruncated       bool
	RequestHeaders     http.Header
	ResponseHeaders    http.Header
	HeadersTruncated   bool
	RequestBody        []byte
	RequestBodyState   CaptureState
	ResponseBody       []byte
	ResponseBodyState  CaptureState
	ResponseStatus     int
	ErrorCode          string
	State              CaptureState
	ReplayIneligible   string
	StartedAt          time.Time
	FinishedAt         time.Time
	EstimatedBytes     int64
}

// Page is one bounded retrieval result.
type Page struct {
	Records    []Record
	NextCursor string
}

// CompletedCapture is the input consumed by Finish. Bodies are raw tapped
// bytes (already bounded by the caller); Finish redacts and enforces caps.
// Truncated flags come from the bounded tap; Unsupported marks streams such as
// WebSocket upgrades or non-JSON bodies for redacted display.
type CompletedCapture struct {
	Method              string
	RawURL              string
	RequestHeaders      http.Header
	ResponseHeaders     http.Header
	RequestBody         []byte
	RequestTruncated    bool
	RequestUnsupported  bool
	RequestIncomplete   bool
	ResponseBody        []byte
	ResponseTruncated   bool
	ResponseUnsupported bool
	ResponseStatus      int
	ErrorCode           string
	ResourceGeneration  uint64
	RouteGeneration     uint64
	TargetGeneration    uint64
	StartedAt           time.Time
	FinishedAt          time.Time
	// RawRequest is the exact raw request bytes for future replay. It is
	// stored separately, never returned by List/Get, and expires after
	// RawRetention. Nil when raw mode is off.
	RawRequest   []byte
	RawTruncated bool
}

// Pending is one in-flight capture slot. It counts against byte/record and
// work-queue budgets until Finish or Abandon releases it.
type Pending struct {
	mu                 sync.Mutex
	ID                 string
	ResourceID         string
	StartedAt          time.Time
	policy             ResourcePolicy
	reservedBytes      int64
	policyEpoch        uint64
	ResourceGeneration uint64
	RouteGeneration    uint64
	TargetGeneration   uint64
	taps               []*BodyTap
	rawTap             *BodyTap
	closed             bool
	rawClosed          bool
}

func (p *Pending) registerTap(tap *BodyTap, raw bool) *BodyTap {
	if p == nil {
		return tap
	}
	p.mu.Lock()
	if p.closed || raw && p.rawClosed {
		tap.release()
	} else {
		p.taps = append(p.taps, tap)
		if raw {
			p.rawTap = tap
		}
	}
	p.mu.Unlock()
	return tap
}

func (p *Pending) releaseTaps() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.closed = true
	p.rawClosed = true
	taps := p.taps
	p.taps = nil
	p.rawTap = nil
	p.mu.Unlock()
	for _, tap := range taps {
		tap.release()
	}
}

func (p *Pending) releaseRawTap() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.rawClosed = true
	tap := p.rawTap
	p.rawTap = nil
	p.mu.Unlock()
	tap.release()
}

type rawEntry struct {
	bytes      []byte
	expiresAt  time.Time
	resourceID string
	size       int64
}

type resourceUsage struct {
	bytes         int64
	count         int
	inflight      int
	inflightBytes int64
	ids           []string
}

// Store is the single daemon-local in-memory capture ring. It never writes
// disk, never contacts the network, and a restart (new Store) deletes all
// captures. All methods are safe for concurrent use.
type Store struct {
	mu                  sync.Mutex
	now                 func() time.Time
	policies            map[string]ResourcePolicy
	policyEpochs        map[string]uint64
	pending             map[string]*Pending
	records             map[string]*Record
	order               []string
	usage               map[string]*resourceUsage
	raw                 map[string]*rawEntry
	daemonBytes         int64
	daemonInflight      int
	daemonInflightBytes int64
	reads               chan struct{}
	wake                chan struct{}
}

func NewStore() *Store {
	return &Store{
		now:          func() time.Time { return time.Now().UTC() },
		policies:     make(map[string]ResourcePolicy),
		policyEpochs: make(map[string]uint64),
		pending:      make(map[string]*Pending),
		records:      make(map[string]*Record),
		usage:        make(map[string]*resourceUsage),
		raw:          make(map[string]*rawEntry),
		reads:        make(chan struct{}, MaxConcurrentReads),
		wake:         make(chan struct{}, 1),
	}
}

func newCaptureID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("inspector: crypto/rand unavailable")
	}
	return hex.EncodeToString(b[:])
}

func (s *Store) currentTime() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func (s *Store) resourceUsageLocked(resourceID string) *resourceUsage {
	u, ok := s.usage[resourceID]
	if !ok {
		u = &resourceUsage{}
		s.usage[resourceID] = u
	}
	return u
}

// SetPolicy installs or replaces the explicit per-resource opt-in. Disabling
// stops new captures; already stored records remain until expiry or Revoke.
func (s *Store) SetPolicy(resourceID string, policy ResourcePolicy) error {
	if !validResourceID(resourceID) {
		return ErrInvalid
	}
	names := make([]string, 0, len(policy.SensitiveNames))
	for _, name := range policy.SensitiveNames {
		if len(names) >= MaxSensitiveNames {
			break
		}
		name = trimName(name)
		if name != "" && len(name) <= 128 {
			names = append(names, name)
		}
	}
	policy.SensitiveNames = names
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.policies[resourceID]; !exists && len(s.policies) >= DaemonMaxRecords {
		return ErrDropped
	}
	s.policyEpochs[resourceID]++
	s.policies[resourceID] = policy
	s.signalCleanupLocked()
	return nil
}

// Revoke immediately denies retrieval and purges every record, raw entry and
// in-flight reservation for the resource.
func (s *Store) Revoke(resourceID string) {
	if !validResourceID(resourceID) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policyEpochs[resourceID]++
	if u, ok := s.usage[resourceID]; ok {
		for _, id := range u.ids {
			if record, ok := s.records[id]; ok {
				s.daemonBytes -= record.EstimatedBytes
				delete(s.records, id)
			}
		}
		for id, pending := range s.pending {
			if pending.ResourceID == resourceID {
				pending.releaseTaps()
				delete(s.pending, id)
			}
		}
		s.daemonInflight -= u.inflight
		s.daemonInflightBytes -= u.inflightBytes
		delete(s.usage, resourceID)
	}
	for id, entry := range s.raw {
		if entry.resourceID == resourceID {
			delete(s.raw, id)
		}
	}
	// Recompute daemon bytes from remaining records to avoid drift between
	// record and raw accounting (raw bytes are included in EstimatedBytes).
	s.recomputeDaemonBytesLocked()
	delete(s.policies, resourceID)
	delete(s.policyEpochs, resourceID)
	s.compactOrderLocked()
	s.signalCleanupLocked()
}

func (s *Store) recomputeDaemonBytesLocked() {
	var total int64
	for _, record := range s.records {
		total += record.EstimatedBytes
	}
	s.daemonBytes = total
}

func (s *Store) compactOrderLocked() {
	kept := s.order[:0]
	for _, id := range s.order {
		if _, ok := s.records[id]; ok {
			kept = append(kept, id)
		}
	}
	s.order = kept
}

// Enabled reports whether new captures are accepted for the resource.
func (s *Store) Enabled(resourceID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	policy, ok := s.policies[resourceID]
	return ok && policy.Enabled && !policy.Revoked
}

// TryBegin reserves one in-flight capture slot without blocking forwarding.
// It returns ErrDisabled when capture is not enabled and ErrDropped when
// work-queue or byte/record budgets cannot admit another in-flight capture.
// Callers must proceed with forwarding in both cases and must call Finish or
// Abandon exactly once on success.
func (s *Store) TryBegin(resourceID string) (*Pending, error) {
	now := s.currentTime()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(now)
	policy, ok := s.policies[resourceID]
	if !ok || !policy.Enabled || policy.Revoked {
		return nil, ErrDisabled
	}
	u := s.resourceUsageLocked(resourceID)
	// URL parsing/stringification can hold the input and an escaped output at
	// once; charge its fixed accepted-input multiple through Finish.
	reserved := int64(MetadataMaxBytes + 4*URLInputMaxBytes)
	if policy.CaptureRequestBody {
		reserved += 4 * BodyMaxBytes // tap plus decoder token/output scratch
	}
	if policy.CaptureResponseBody {
		reserved += 4 * BodyMaxBytes // tap plus decoder token/output scratch
	}
	if policy.CaptureRaw {
		reserved += RawMaxBytes
	}
	if s.daemonInflight >= WorkQueueDaemonMax || u.inflight >= WorkQueueResourceMax {
		return nil, ErrDropped
	}
	// A ring at its completed bound remains live: evict completed oldest
	// entries before deciding that only active reservations remain.
	s.evictForAdmissionLocked(resourceID, reserved)
	// Eviction may have removed this resource's last completed record and
	// pruned its usage entry; this admission continues to own u.
	s.usage[resourceID] = u
	if len(s.records)+s.daemonInflight >= DaemonMaxRecords || u.count+u.inflight >= ResourceMaxRecords {
		return nil, ErrDropped
	}
	if s.daemonBytes+s.daemonInflightBytes+reserved > DaemonMaxBytes ||
		u.bytes+u.inflightBytes+reserved > ResourceMaxBytes {
		return nil, ErrDropped
	}
	pending := &Pending{
		ID:                 newCaptureID(),
		ResourceID:         resourceID,
		StartedAt:          now,
		policy:             policy,
		reservedBytes:      reserved,
		policyEpoch:        s.policyEpochs[resourceID],
		ResourceGeneration: policy.ResourceGeneration,
		RouteGeneration:    policy.RouteGeneration,
		TargetGeneration:   policy.TargetGeneration,
	}
	s.daemonInflight++
	s.daemonInflightBytes += reserved
	u.inflight++
	u.inflightBytes += reserved
	s.pending[pending.ID] = pending
	s.signalCleanupLocked()
	return pending, nil
}

func (s *Store) evictForAdmissionLocked(resourceID string, need int64) {
	for (len(s.records)+s.daemonInflight >= DaemonMaxRecords ||
		s.daemonBytes+s.daemonInflightBytes+need > DaemonMaxBytes) && len(s.order) > 0 {
		s.removeRecordLocked(s.order[0])
	}
	if u := s.usage[resourceID]; u != nil {
		for (u.count+u.inflight >= ResourceMaxRecords ||
			u.bytes+u.inflightBytes+need > ResourceMaxBytes) && len(u.ids) > 0 {
			s.removeRecordLocked(u.ids[0])
		}
	}
}

// Abandon releases an in-flight slot without storing a record, e.g. when the
// forwarded exchange never produced a parsable request.
func (s *Store) Abandon(pending *Pending) {
	if pending == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releasePendingLocked(pending)
}

// releasePendingLocked releases exactly the reservation originally admitted.
// Late/double completion after expiry, revocation, or re-enable is a no-op.
func (s *Store) releasePendingLocked(pending *Pending) bool {
	active, ok := s.pending[pending.ID]
	if !ok || active != pending {
		return false
	}
	delete(s.pending, pending.ID)
	pending.releaseTaps()
	u, ok := s.usage[pending.ResourceID]
	if !ok {
		return false
	}
	if u.inflight > 0 {
		u.inflight--
	}
	if u.inflightBytes >= pending.reservedBytes {
		u.inflightBytes -= pending.reservedBytes
	} else {
		u.inflightBytes = 0
	}
	if s.daemonInflight > 0 {
		s.daemonInflight--
	}
	if s.daemonInflightBytes >= pending.reservedBytes {
		s.daemonInflightBytes -= pending.reservedBytes
	} else {
		s.daemonInflightBytes = 0
	}
	s.cleanupUsageLocked(pending.ResourceID, u)
	return true
}

func (s *Store) cleanupUsageLocked(resourceID string, usage *resourceUsage) {
	if usage != nil && usage.count == 0 && usage.inflight == 0 {
		delete(s.usage, resourceID)
	}
}

func trimName(value string) string {
	if len(value) > 128 {
		value = value[:128]
	}
	return value
}

// Finish sanitizes a completed exchange and stores it subject to budgets. It
// releases the in-flight reservation in all cases. Over-budget inserts evict
// the oldest stored records in the exceeded scope; when eviction cannot free
// enough room the capture is dropped and ErrDropped is returned. Forwarding
// has already completed by this point, so dropping never blocks traffic.
func (s *Store) Finish(pending *Pending, completed CompletedCapture) (*Record, error) {
	if pending == nil {
		return nil, ErrInvalid
	}
	now := s.currentTime()
	s.mu.Lock()
	defer s.mu.Unlock()
	active, activeOK := s.pending[pending.ID]
	if !activeOK || active != pending {
		return nil, ErrDisabled
	}
	if !now.Before(pending.StartedAt.Add(Retention)) {
		s.releasePendingLocked(pending)
		return nil, ErrExpired
	}
	policy, ok := s.policies[pending.ResourceID]
	if !ok || !policy.Enabled || policy.Revoked || pending.policyEpoch != s.policyEpochs[pending.ResourceID] {
		s.releasePendingLocked(pending)
		return nil, ErrDisabled
	}
	record := buildRecord(pending, policy, completed, now)
	s.releasePendingLocked(pending)
	u := s.resourceUsageLocked(pending.ResourceID)
	if record.EstimatedBytes > ResourceMaxBytes || record.EstimatedBytes > DaemonMaxBytes {
		return nil, ErrDropped
	}
	s.evictForInsertLocked(pending.ResourceID, record.EstimatedBytes)
	if len(s.records)+s.daemonInflight >= DaemonMaxRecords || u.count+u.inflight >= ResourceMaxRecords ||
		s.daemonBytes+s.daemonInflightBytes+record.EstimatedBytes > DaemonMaxBytes ||
		u.bytes+u.inflightBytes+record.EstimatedBytes > ResourceMaxBytes {
		return nil, ErrDropped
	}
	s.records[record.ID] = record
	s.order = append(s.order, record.ID)
	u.bytes += record.EstimatedBytes
	u.count++
	u.ids = append(u.ids, record.ID)
	s.daemonBytes += record.EstimatedBytes
	if len(completed.RawRequest) > 0 && policy.CaptureRaw && record.ReplayIneligible == "" {
		// Finish consumes RawRequest, normally transferred from BodyTap.Take,
		// so retaining it does not create a second full raw buffer.
		raw := completed.RawRequest
		s.raw[record.ID] = &rawEntry{bytes: raw, expiresAt: record.StartedAt.Add(RawRetention), resourceID: record.ResourceID, size: int64(len(raw))}
	} else if len(completed.RawRequest) > 0 && policy.CaptureRaw {
		// Raw bytes still count against budgets only when retained; a
		// truncated or ineligible raw is not retained.
	}
	s.signalCleanupLocked()
	return record, nil
}

// evictForInsertLocked removes the oldest stored records in the exceeded scope
// until the new record fits or no stored records remain in that scope.
func (s *Store) evictForInsertLocked(resourceID string, need int64) {
	for len(s.order) > 0 && (len(s.records)+s.daemonInflight >= DaemonMaxRecords || s.daemonBytes+s.daemonInflightBytes+need > DaemonMaxBytes) {
		oldest := s.order[0]
		s.removeRecordLocked(oldest)
	}
	if u, ok := s.usage[resourceID]; ok {
		for len(u.ids) > 0 && (u.count+u.inflight >= ResourceMaxRecords || u.bytes+u.inflightBytes+need > ResourceMaxBytes) {
			oldest := u.ids[0]
			s.removeRecordLocked(oldest)
		}
	}
}

func (s *Store) removeRecordLocked(id string) {
	record, ok := s.records[id]
	if !ok {
		return
	}
	delete(s.records, id)
	delete(s.raw, id)
	s.daemonBytes -= record.EstimatedBytes
	if u, ok := s.usage[record.ResourceID]; ok {
		u.bytes -= record.EstimatedBytes
		u.count--
		for i, candidate := range u.ids {
			if candidate == id {
				u.ids = append(u.ids[:i], u.ids[i+1:]...)
				break
			}
		}
		s.cleanupUsageLocked(record.ResourceID, u)
	}
	for i, candidate := range s.order {
		if candidate == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// expireLocked purges retention-expired records and independently expired raw
// bytes. Raw expiry marks the surviving sanitized record ineligible for replay.
func (s *Store) expireLocked(now time.Time) {
	for _, pending := range s.pending {
		if !now.Before(pending.StartedAt.Add(RawRetention)) {
			pending.releaseRawTap()
		}
		if !now.Before(pending.StartedAt.Add(Retention)) {
			s.releasePendingLocked(pending)
		}
	}
	for id, record := range s.records {
		if !now.Before(record.StartedAt.Add(Retention)) {
			s.removeRecordLocked(id)
		}
	}
	for id, entry := range s.raw {
		if !now.Before(entry.expiresAt) {
			delete(s.raw, id)
			if record, ok := s.records[id]; ok && record.ReplayIneligible == "" {
				record.ReplayIneligible = "raw_expired"
				record.EstimatedBytes -= entry.size
				s.daemonBytes -= entry.size
				if usage := s.usage[record.ResourceID]; usage != nil {
					usage.bytes -= entry.size
				}
			}
		}
	}
}

func (s *Store) signalCleanupLocked() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Store) nextExpiryLocked(now time.Time) time.Time {
	var next time.Time
	consider := func(candidate time.Time) {
		if !candidate.After(now) {
			candidate = now
		}
		if next.IsZero() || candidate.Before(next) {
			next = candidate
		}
	}
	for _, pending := range s.pending {
		pending.mu.Lock()
		rawClosed := pending.rawClosed
		pending.mu.Unlock()
		if !rawClosed {
			consider(pending.StartedAt.Add(RawRetention))
		}
		consider(pending.StartedAt.Add(Retention))
	}
	for id, record := range s.records {
		consider(record.StartedAt.Add(Retention))
		if entry := s.raw[id]; entry != nil {
			consider(entry.expiresAt)
		}
	}
	return next
}

func (s *Store) purgeLocked() {
	for _, pending := range s.pending {
		pending.releaseTaps()
	}
	s.policies = make(map[string]ResourcePolicy)
	s.policyEpochs = make(map[string]uint64)
	s.pending = make(map[string]*Pending)
	s.records = make(map[string]*Record)
	s.order = nil
	s.usage = make(map[string]*resourceUsage)
	s.raw = make(map[string]*rawEntry)
	s.daemonBytes, s.daemonInflight, s.daemonInflightBytes = 0, 0, 0
}

// Run performs exact-deadline cleanup while the daemon is otherwise idle.
// NewStore starts no background work; cancellation purges daemon-local state.
func (s *Store) Run(ctx context.Context) {
	if ctx == nil {
		return
	}
	for {
		s.mu.Lock()
		now := s.currentTime()
		s.expireLocked(now)
		next := s.nextExpiryLocked(now)
		s.mu.Unlock()
		wait := time.Hour
		if !next.IsZero() {
			wait = next.Sub(now)
		}
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			s.mu.Lock()
			s.purgeLocked()
			s.mu.Unlock()
			return
		case <-s.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
	}
}

// authorizeRetrieval enforces action separation, revocation, freshness and
// generation fencing. Viewers never pass; inspect opens List/Get and replay
// opens GetRaw.
func (s *Store) authorizeRetrieval(credential Credential, want Action) (ResourcePolicy, error) {
	now := s.currentTime()
	if !validResourceID(credential.ResourceID) || credential.PrincipalID == "" {
		return ResourcePolicy{}, ErrInvalid
	}
	if credential.Action != want {
		return ResourcePolicy{}, ErrForbidden
	}
	if credential.AuthorityReadAt.IsZero() || credential.ExpiresAt.IsZero() ||
		!credential.ExpiresAt.After(now) || now.Sub(credential.AuthorityReadAt) > AuthorityFreshness {
		return ResourcePolicy{}, ErrStaleAuthority
	}
	policy, ok := s.policies[credential.ResourceID]
	if !ok || policy.Revoked {
		return ResourcePolicy{}, ErrStaleAuthority
	}
	if policy.ResourceGeneration != 0 && credential.ResourceGeneration != policy.ResourceGeneration ||
		policy.RouteGeneration != 0 && credential.RouteGeneration != policy.RouteGeneration ||
		policy.TargetGeneration != 0 && credential.TargetGeneration != policy.TargetGeneration {
		return ResourcePolicy{}, ErrStaleGeneration
	}
	return policy, nil
}

func (s *Store) acquireRead(ctx context.Context) error {
	select {
	case s.reads <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(SlowReadTimeout):
		return ErrTooManyReads
	}
}

func (s *Store) releaseRead() {
	select {
	case <-s.reads:
	default:
	}
}

// List returns one bounded page of sanitized records oldest-first. Raw bytes
// are never included.
func (s *Store) List(ctx context.Context, credential Credential, cursor string, limit int) (Page, error) {
	if ctx == nil {
		return Page{}, ErrInvalid
	}
	if limit <= 0 || limit > RetrievalMaxRecords {
		return Page{}, ErrInvalid
	}
	if err := s.acquireRead(ctx); err != nil {
		return Page{}, err
	}
	defer s.releaseRead()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.currentTime()
	s.expireLocked(now)
	if _, err := s.authorizeRetrieval(credential, ActionInspect); err != nil {
		return Page{}, err
	}
	ordered := make([]*Record, 0, len(s.records))
	for _, record := range s.records {
		if record.ResourceID != credential.ResourceID || staleRecord(record, credential) {
			continue
		}
		ordered = append(ordered, record)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].StartedAt.Equal(ordered[j].StartedAt) {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].StartedAt.Before(ordered[j].StartedAt)
	})
	start := 0
	if cursor != "" {
		found := false
		for i, record := range ordered {
			if record.ID == cursor {
				start = i + 1
				found = true
				break
			}
		}
		if !found {
			return Page{}, ErrInvalid
		}
	}
	page := Page{}
	var bytes int64
	for _, record := range ordered[start:] {
		if len(page.Records) >= limit {
			page.NextCursor = page.Records[len(page.Records)-1].ID
			break
		}
		if len(page.Records) > 0 && bytes+record.EstimatedBytes > RetrievalMaxBytes {
			page.NextCursor = page.Records[len(page.Records)-1].ID
			break
		}
		page.Records = append(page.Records, *record)
		bytes += record.EstimatedBytes
	}
	return page, nil
}

func staleRecord(record *Record, credential Credential) bool {
	if credential.ResourceGeneration != 0 && record.ResourceGeneration != credential.ResourceGeneration {
		return true
	}
	if credential.RouteGeneration != 0 && record.RouteGeneration != credential.RouteGeneration {
		return true
	}
	if credential.TargetGeneration != 0 && record.TargetGeneration != credential.TargetGeneration {
		return true
	}
	return false
}

// Get returns one sanitized record without raw bytes.
func (s *Store) Get(ctx context.Context, credential Credential, id string) (Record, error) {
	if ctx == nil || id == "" {
		return Record{}, ErrInvalid
	}
	if err := s.acquireRead(ctx); err != nil {
		return Record{}, err
	}
	defer s.releaseRead()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.currentTime()
	s.expireLocked(now)
	if _, err := s.authorizeRetrieval(credential, ActionInspect); err != nil {
		return Record{}, err
	}
	record, ok := s.records[id]
	if !ok || record.ResourceID != credential.ResourceID {
		return Record{}, ErrNotFound
	}
	if staleRecord(record, credential) {
		return Record{}, ErrStaleGeneration
	}
	return *record, nil
}

// GetRaw returns the retained exact raw request bytes for a future deliberate
// replay. It requires a replay credential and complete retained raw; Task 32
// owns the actual replay operation. Raw bytes are never part of List/Get.
func (s *Store) GetRaw(ctx context.Context, credential Credential, id string) ([]byte, error) {
	if ctx == nil || id == "" {
		return nil, ErrInvalid
	}
	if err := s.acquireRead(ctx); err != nil {
		return nil, err
	}
	defer s.releaseRead()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.currentTime()
	s.expireLocked(now)
	if _, err := s.authorizeRetrieval(credential, ActionReplay); err != nil {
		return nil, err
	}
	record, ok := s.records[id]
	if !ok || record.ResourceID != credential.ResourceID {
		return nil, ErrNotFound
	}
	if staleRecord(record, credential) {
		return nil, ErrStaleGeneration
	}
	if record.ReplayIneligible != "" {
		return nil, ErrExpired
	}
	entry, ok := s.raw[id]
	if !ok {
		return nil, ErrExpired
	}
	if now.After(entry.expiresAt) {
		delete(s.raw, id)
		record.ReplayIneligible = "raw_expired"
		return nil, ErrExpired
	}
	out := make([]byte, len(entry.bytes))
	copy(out, entry.bytes)
	return out, nil
}

// Stats reports current daemon and per-resource usage for tests and status.
type Stats struct {
	DaemonBytes   int64
	DaemonRecords int
	ResourceBytes map[string]int64
}

func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(s.currentTime())
	out := Stats{DaemonRecords: len(s.records) + s.daemonInflight, ResourceBytes: make(map[string]int64)}
	var total int64
	for _, record := range s.records {
		total += record.EstimatedBytes
	}
	out.DaemonBytes = total + s.daemonInflightBytes
	for id, u := range s.usage {
		out.ResourceBytes[id] = u.bytes + u.inflightBytes
	}
	return out
}
