// Package health is the one definition of Paperboat's typed health model. The host
// runtime, the edge and the control plane all track and export it, so one decoder
// reads health from every side of the product.
//
// Prior art: tailscale health/health.go (one Tracker imported by every binary).
package health

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/telemetry/safe"
)

// HealthSchemaV1 identifies the exported snapshot.
const HealthSchemaV1 = "paperboat.health/v1"

// Dimension is one independently actionable subsystem. Keep dimensionOrder
// stable: it is also the canonical JSON order and the overall tie-break.
type Dimension string

const (
	DimensionService     Dimension = "service"
	DimensionEdge        Dimension = "edge"
	DimensionConfig      Dimension = "config"
	DimensionRoute       Dimension = "route"
	DimensionOrigin      Dimension = "origin"
	DimensionDNS         Dimension = "dns"
	DimensionCertificate Dimension = "certificate"
	DimensionAccess      Dimension = "access"
	DimensionUpdate      Dimension = "update"
)

var dimensionOrder = []Dimension{
	DimensionService,
	DimensionEdge,
	DimensionConfig,
	DimensionRoute,
	DimensionOrigin,
	DimensionDNS,
	DimensionCertificate,
	DimensionAccess,
	DimensionUpdate,
}

// Dimensions returns the canonical order used in JSON, metrics and overall
// tie-breaking. The returned slice is independent of package state.
func Dimensions() []Dimension { return append([]Dimension(nil), dimensionOrder...) }

type HealthStatus string

const (
	StatusUnknown       HealthStatus = "unknown"
	StatusReady         HealthStatus = "ready"
	StatusDegraded      HealthStatus = "degraded"
	StatusDown          HealthStatus = "down"
	StatusNotApplicable HealthStatus = "not_applicable"
)

type RetryDecision string

const (
	RetryNone          RetryDecision = "none"
	RetryScheduled     RetryDecision = "scheduled"
	RetryWaitForChange RetryDecision = "wait_for_change"
	RetryNotRetryable  RetryDecision = "not_retryable"
)

type HealthUpdate struct {
	Dimension     Dimension
	Status        HealthStatus
	Code          string
	Summary       string
	RepairAction  string
	CorrelationID string
	Retry         RetryDecision
	NextRetryAt   time.Time
}

type DimensionHealth struct {
	Status        HealthStatus  `json:"status"`
	Code          string        `json:"code"`
	Since         time.Time     `json:"since"`
	BrokenSince   *time.Time    `json:"broken_since,omitempty"`
	Summary       string        `json:"summary"`
	RepairAction  string        `json:"repair_action"`
	CorrelationID string        `json:"correlation_id,omitempty"`
	Retry         RetryDecision `json:"retry"`
	NextRetryAt   *time.Time    `json:"next_retry_at,omitempty"`
	SuppressedBy  Dimension     `json:"suppressed_by,omitempty"`
}

type HealthDimensions struct {
	Service     DimensionHealth `json:"service"`
	Edge        DimensionHealth `json:"edge"`
	Config      DimensionHealth `json:"config"`
	Route       DimensionHealth `json:"route"`
	Origin      DimensionHealth `json:"origin"`
	DNS         DimensionHealth `json:"dns"`
	Certificate DimensionHealth `json:"certificate"`
	Access      DimensionHealth `json:"access"`
	Update      DimensionHealth `json:"update"`
}

func (d HealthDimensions) Get(dimension Dimension) DimensionHealth {
	switch dimension {
	case DimensionService:
		return d.Service
	case DimensionEdge:
		return d.Edge
	case DimensionConfig:
		return d.Config
	case DimensionRoute:
		return d.Route
	case DimensionOrigin:
		return d.Origin
	case DimensionDNS:
		return d.DNS
	case DimensionCertificate:
		return d.Certificate
	case DimensionAccess:
		return d.Access
	case DimensionUpdate:
		return d.Update
	default:
		return DimensionHealth{}
	}
}

func (d *HealthDimensions) set(dimension Dimension, value DimensionHealth) {
	switch dimension {
	case DimensionService:
		d.Service = value
	case DimensionEdge:
		d.Edge = value
	case DimensionConfig:
		d.Config = value
	case DimensionRoute:
		d.Route = value
	case DimensionOrigin:
		d.Origin = value
	case DimensionDNS:
		d.DNS = value
	case DimensionCertificate:
		d.Certificate = value
	case DimensionAccess:
		d.Access = value
	case DimensionUpdate:
		d.Update = value
	}
}

type OverallHealth struct {
	Status        HealthStatus  `json:"status"`
	Code          string        `json:"code"`
	Dimension     Dimension     `json:"dimension,omitempty"`
	Since         time.Time     `json:"since"`
	BrokenSince   *time.Time    `json:"broken_since,omitempty"`
	Summary       string        `json:"summary"`
	RepairAction  string        `json:"repair_action"`
	CorrelationID string        `json:"correlation_id,omitempty"`
	Retry         RetryDecision `json:"retry"`
	NextRetryAt   *time.Time    `json:"next_retry_at,omitempty"`
}

type HealthSnapshot struct {
	Schema     string           `json:"schema"`
	UpdatedAt  time.Time        `json:"updated_at"`
	Overall    OverallHealth    `json:"overall"`
	Dimensions HealthDimensions `json:"dimensions"`
	ETag       string           `json:"etag"`
}

func (s HealthSnapshot) JSON() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(s)
}

// Validate checks the complete v1 snapshot before it is exported. The ETag is
// required: a snapshot that did not come from a tracker is not exportable.
func (s HealthSnapshot) Validate() error {
	if s.Schema != HealthSchemaV1 || safe.NormalizeTime(s.UpdatedAt).IsZero() || !ValidHealthStatus(s.Overall.Status) || !safe.StableCode(s.Overall.Code) || safe.NormalizeTime(s.Overall.Since).IsZero() || !safe.StoredText(s.Overall.Summary, safe.MaximumSummaryBytes) || !safe.StoredText(s.Overall.RepairAction, safe.MaximumRepairBytes) || !ValidRetry(s.Overall.Retry, safe.TimeValue(s.Overall.NextRetryAt)) {
		return safe.NewError(safe.ErrorInvalidObservation, "health")
	}
	if s.Overall.Dimension != "" && !ValidDimension(s.Overall.Dimension) {
		return safe.NewError(safe.ErrorInvalidDimension, "health")
	}
	if s.Overall.CorrelationID != "" && !safe.CorrelationID(s.Overall.CorrelationID) {
		return safe.NewError(safe.ErrorInvalidID, "health")
	}
	if isBroken(s.Overall.Status) != (s.Overall.BrokenSince != nil) {
		return safe.NewError(safe.ErrorInvalidObservation, "health")
	}
	for _, dimension := range dimensionOrder {
		state := s.Dimensions.Get(dimension)
		if !ValidHealthStatus(state.Status) || !safe.StableCode(state.Code) || safe.NormalizeTime(state.Since).IsZero() || !safe.StoredText(state.Summary, safe.MaximumSummaryBytes) || !safe.StoredText(state.RepairAction, safe.MaximumRepairBytes) || !ValidRetry(state.Retry, safe.TimeValue(state.NextRetryAt)) {
			return safe.NewError(safe.ErrorInvalidObservation, "health")
		}
		if state.CorrelationID != "" && !safe.CorrelationID(state.CorrelationID) {
			return safe.NewError(safe.ErrorInvalidID, "health")
		}
		if state.SuppressedBy != "" && (!ValidDimension(state.SuppressedBy) || state.SuppressedBy == dimension) {
			return safe.NewError(safe.ErrorInvalidDimension, "health")
		}
		if isBroken(state.Status) != (state.BrokenSince != nil) {
			return safe.NewError(safe.ErrorInvalidObservation, "health")
		}
	}
	if !etagPattern.MatchString(s.ETag) {
		return safe.NewError(safe.ErrorInvalidObservation, "health")
	}
	return nil
}

var etagPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Alert is a safe, actionable projection suitable for a pager or notification
// adapter. It contains no raw provider or request data.
type Alert struct {
	At            time.Time     `json:"at"`
	Dimension     Dimension     `json:"dimension"`
	Status        HealthStatus  `json:"status"`
	Code          string        `json:"code"`
	BrokenSince   *time.Time    `json:"broken_since,omitempty"`
	Summary       string        `json:"summary"`
	RepairAction  string        `json:"repair_action"`
	CorrelationID string        `json:"correlation_id,omitempty"`
	Retry         RetryDecision `json:"retry"`
	NextRetryAt   *time.Time    `json:"next_retry_at,omitempty"`
	Correlation   string        `json:"-"`
}

// AlertFor returns an immutable-safe alert projection for one dimension.
func (s HealthSnapshot) AlertFor(dimension Dimension) (Alert, bool) {
	if !ValidDimension(dimension) {
		return Alert{}, false
	}
	state := s.Dimensions.Get(dimension)
	if state.SuppressedBy != "" || state.Status == StatusReady || state.Status == StatusNotApplicable {
		return Alert{}, false
	}
	return Alert{At: s.UpdatedAt, Dimension: dimension, Status: state.Status, Code: state.Code, BrokenSince: safe.CloneTime(state.BrokenSince), Summary: state.Summary, RepairAction: state.RepairAction, CorrelationID: state.CorrelationID, Correlation: state.CorrelationID, Retry: state.Retry, NextRetryAt: safe.CloneTime(state.NextRetryAt)}, true
}

// Alerts returns only deterministic actionable root alerts. Failures hidden by
// a down dependency are omitted so operators receive one repair target.
func (s HealthSnapshot) Alerts() []Alert {
	alerts := make([]Alert, 0, len(dimensionOrder))
	for _, dimension := range dimensionOrder {
		if alert, ok := s.AlertFor(dimension); ok {
			alerts = append(alerts, alert)
		}
	}
	return alerts
}

type HealthTracker struct {
	mu           sync.RWMutex
	now          func() time.Time
	readySummary string
	updatedAt    time.Time
	states       HealthDimensions
}

// NewHealthTracker returns a tracker whose overall status reads readySummary
// when every applicable dimension is ready (for example "All applicable edge
// health dimensions are ready."). now is injected for tests.
func NewHealthTracker(now func() time.Time, readySummary string) (*HealthTracker, error) {
	if now == nil {
		now = time.Now
	}
	at := safe.NormalizeTime(now())
	if at.IsZero() {
		return nil, safe.NewError(safe.ErrorInvalidTime, "health")
	}
	unknown := DimensionHealth{
		Status:       StatusUnknown,
		Code:         "not_observed",
		Since:        at,
		Summary:      "Health has not been observed.",
		RepairAction: "Wait for the first health observation.",
		Retry:        RetryNone,
	}
	states := HealthDimensions{}
	for _, dimension := range dimensionOrder {
		states.set(dimension, unknown)
	}
	return &HealthTracker{now: now, readySummary: readySummary, updatedAt: at, states: states}, nil
}

func (t *HealthTracker) Update(update HealthUpdate) error {
	if t == nil {
		return safe.NewError(safe.ErrorInvalidObservation, "health")
	}
	prepared, err := prepareHealthUpdate(update)
	if err != nil {
		return err
	}
	at := safe.NormalizeTime(t.now())
	if at.IsZero() {
		return safe.NewError(safe.ErrorInvalidTime, "health")
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	previous := t.states.Get(update.Dimension)
	next := DimensionHealth{
		Status:        prepared.Status,
		Code:          prepared.Code,
		Since:         at,
		Summary:       prepared.Summary,
		RepairAction:  prepared.RepairAction,
		CorrelationID: prepared.CorrelationID,
		Retry:         prepared.Retry,
	}
	if !prepared.NextRetryAt.IsZero() {
		nextRetry := safe.NormalizeTime(prepared.NextRetryAt)
		next.NextRetryAt = &nextRetry
	}
	if previous.Status == next.Status && previous.Code == next.Code {
		next.Since = previous.Since
	}
	if isBroken(next.Status) {
		brokenAt := at
		if previous.BrokenSince != nil {
			brokenAt = *previous.BrokenSince
		}
		next.BrokenSince = &brokenAt
	}
	if dimensionHealthEqual(previous, next) {
		return nil
	}
	t.states.set(update.Dimension, next)
	t.updatedAt = at
	return nil
}

func (t *HealthTracker) Snapshot() HealthSnapshot {
	if t == nil {
		return HealthSnapshot{}
	}
	t.mu.RLock()
	states := cloneHealthDimensions(t.states)
	updatedAt := t.updatedAt
	t.mu.RUnlock()

	states = suppressDependencies(states)
	snapshot := HealthSnapshot{
		Schema:     HealthSchemaV1,
		UpdatedAt:  updatedAt,
		Overall:    projectOverall(states, updatedAt, t.readySummary),
		Dimensions: states,
	}
	withoutETag, _ := json.Marshal(snapshot)
	digest := sha256.Sum256(withoutETag)
	snapshot.ETag = "sha256:" + hex.EncodeToString(digest[:])
	return snapshot
}

func cloneHealthDimensions(states HealthDimensions) HealthDimensions {
	for _, dimension := range dimensionOrder {
		state := states.Get(dimension)
		state.BrokenSince = safe.CloneTime(state.BrokenSince)
		state.NextRetryAt = safe.CloneTime(state.NextRetryAt)
		states.set(dimension, state)
	}
	return states
}

func prepareHealthUpdate(update HealthUpdate) (HealthUpdate, error) {
	if !ValidDimension(update.Dimension) {
		return HealthUpdate{}, safe.NewError(safe.ErrorInvalidDimension, "health")
	}
	if !ValidHealthStatus(update.Status) {
		return HealthUpdate{}, safe.NewError(safe.ErrorInvalidStatus, "health")
	}
	if !safe.StableCode(update.Code) {
		return HealthUpdate{}, safe.NewError(safe.ErrorInvalidCode, "health")
	}
	if !ValidRetry(update.Retry, update.NextRetryAt) {
		return HealthUpdate{}, safe.NewError(safe.ErrorInvalidRetry, "health")
	}
	summary, err := safe.BoundedString(update.Summary, safe.MaximumSummaryBytes, true)
	if err != nil {
		return HealthUpdate{}, err
	}
	repair, err := safe.BoundedString(update.RepairAction, safe.MaximumRepairBytes, true)
	if err != nil {
		return HealthUpdate{}, err
	}
	if update.CorrelationID != "" && !safe.CorrelationID(update.CorrelationID) {
		return HealthUpdate{}, safe.NewError(safe.ErrorInvalidID, "health")
	}
	update.Summary = summary
	update.RepairAction = repair
	update.NextRetryAt = safe.NormalizeTime(update.NextRetryAt)
	return update, nil
}

// ValidDimension reports whether value is a known dimension.
func ValidDimension(value Dimension) bool {
	for _, dimension := range dimensionOrder {
		if value == dimension {
			return true
		}
	}
	return false
}

// ValidHealthStatus reports whether value is a known status.
func ValidHealthStatus(value HealthStatus) bool {
	return value == StatusUnknown || value == StatusReady || value == StatusDegraded || value == StatusDown || value == StatusNotApplicable
}

// ValidRetry reports whether decision and next agree: only a scheduled retry carries a time.
func ValidRetry(decision RetryDecision, next time.Time) bool {
	switch decision {
	case RetryScheduled:
		return !next.IsZero()
	case RetryNone, RetryWaitForChange, RetryNotRetryable:
		return next.IsZero()
	default:
		return false
	}
}

func isBroken(status HealthStatus) bool { return status == StatusDegraded || status == StatusDown }

func dimensionHealthEqual(left, right DimensionHealth) bool {
	left.SuppressedBy, right.SuppressedBy = "", ""
	return left.Status == right.Status && left.Code == right.Code && left.Since.Equal(right.Since) &&
		timesEqual(left.BrokenSince, right.BrokenSince) && left.Summary == right.Summary &&
		left.RepairAction == right.RepairAction && left.CorrelationID == right.CorrelationID &&
		left.Retry == right.Retry && timesEqual(left.NextRetryAt, right.NextRetryAt)
}

func timesEqual(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

// A down dependency suppresses dependent noise. Degraded dependencies stay
// visible because they may still be actionable independently.
var dimensionDependencies = map[Dimension][]Dimension{
	DimensionEdge:        {DimensionService},
	DimensionConfig:      {DimensionService},
	DimensionRoute:       {DimensionService, DimensionEdge, DimensionConfig},
	DimensionOrigin:      {DimensionService, DimensionEdge, DimensionConfig, DimensionRoute},
	DimensionDNS:         {DimensionService, DimensionEdge, DimensionConfig, DimensionRoute},
	DimensionCertificate: {DimensionService, DimensionEdge, DimensionConfig, DimensionRoute, DimensionDNS},
	DimensionAccess:      {DimensionService, DimensionEdge, DimensionConfig, DimensionRoute},
	DimensionUpdate:      {DimensionService},
}

func suppressDependencies(states HealthDimensions) HealthDimensions {
	for _, dimension := range dimensionOrder {
		state := states.Get(dimension)
		state.SuppressedBy = dependencyRoot(states, dimension, make(map[Dimension]bool))
		states.set(dimension, state)
	}
	return states
}

// dependencyRoot follows the dependency graph to the first independently
// actionable down dimension. Returning the root, not the local symptom, keeps
// overall health and alerting stable as more descendants see the same outage.
func dependencyRoot(states HealthDimensions, dimension Dimension, visiting map[Dimension]bool) Dimension {
	if visiting[dimension] {
		return ""
	}
	visiting[dimension] = true
	defer delete(visiting, dimension)
	for _, dependency := range dimensionDependencies[dimension] {
		if states.Get(dependency).Status != StatusDown {
			continue
		}
		if root := dependencyRoot(states, dependency, visiting); root != "" {
			return root
		}
		return dependency
	}
	return ""
}

func projectOverall(states HealthDimensions, updatedAt time.Time, readySummary string) OverallHealth {
	type candidate struct {
		dimension Dimension
		state     DimensionHealth
		severity  int
		priority  int
	}
	candidates := make([]candidate, 0, len(dimensionOrder))
	for priority, dimension := range dimensionOrder {
		state := states.Get(dimension)
		if state.SuppressedBy != "" || state.Status == StatusReady || state.Status == StatusNotApplicable {
			continue
		}
		severity := map[HealthStatus]int{StatusUnknown: 1, StatusDegraded: 2, StatusDown: 3}[state.Status]
		candidates = append(candidates, candidate{dimension: dimension, state: state, severity: severity, priority: priority})
	}
	if len(candidates) == 0 {
		return OverallHealth{
			Status:       StatusReady,
			Code:         "ready",
			Since:        updatedAt,
			Summary:      readySummary,
			RepairAction: "No action is required.",
			Retry:        RetryNone,
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].severity != candidates[j].severity {
			return candidates[i].severity > candidates[j].severity
		}
		return candidates[i].priority < candidates[j].priority
	})
	selected := candidates[0]
	return OverallHealth{
		Status:        selected.state.Status,
		Code:          selected.state.Code,
		Dimension:     selected.dimension,
		Since:         selected.state.Since,
		BrokenSince:   safe.CloneTime(selected.state.BrokenSince),
		Summary:       selected.state.Summary,
		RepairAction:  selected.state.RepairAction,
		CorrelationID: selected.state.CorrelationID,
		Retry:         selected.state.Retry,
		NextRetryAt:   safe.CloneTime(selected.state.NextRetryAt),
	}
}
