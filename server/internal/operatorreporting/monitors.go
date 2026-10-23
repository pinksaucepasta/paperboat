package operatorreporting

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"strconv"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
)

type Monitor struct {
	Project           string               `json:"project"`
	Slug              string               `json:"slug"`
	Name              string               `json:"name,omitempty"`
	Status            string               `json:"status,omitempty"`
	Type              string               `json:"type,omitempty"`
	Enabled           bool                 `json:"enabled"`
	LastCheckIn       time.Time            `json:"last_check_in,omitempty"`
	NextExpected      time.Time            `json:"next_expected,omitempty"`
	CheckInMargin     int64                `json:"check_in_margin,omitempty"`
	MaxRuntime        int64                `json:"max_runtime,omitempty"`
	FailureThreshold  int64                `json:"failure_threshold,omitempty"`
	RecoveryThreshold int64                `json:"recovery_threshold,omitempty"`
	Environments      []MonitorEnvironment `json:"environments"`
}
type MonitorEnvironment struct {
	Name         string           `json:"name"`
	Status       string           `json:"status,omitempty"`
	LastCheckIn  time.Time        `json:"last_check_in,omitempty"`
	NextExpected time.Time        `json:"next_expected,omitempty"`
	NextLatest   time.Time        `json:"next_latest,omitempty"`
	Incident     *MonitorIncident `json:"incident,omitempty"`
}
type MonitorIncident struct {
	StartedAt   time.Time  `json:"started_at"`
	ResolvingAt *time.Time `json:"resolving_at,omitempty"`
}
type MonitorPage struct {
	Monitors   []Monitor `json:"monitors"`
	NextCursor string    `json:"next_cursor,omitempty"`
}
type CheckIn struct {
	ID          string    `json:"id"`
	Status      string    `json:"status"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	FinishedAt  time.Time `json:"finished_at,omitempty"`
	DurationMS  int64     `json:"duration_ms,omitempty"`
	Environment string    `json:"environment,omitempty"`
}
type CheckInPage struct {
	CheckIns   []CheckIn `json:"check_ins"`
	NextCursor string    `json:"next_cursor,omitempty"`
}
type Detector struct {
	Project           string  `json:"project"`
	ID                string  `json:"id"`
	IssueID           string  `json:"issue_id,omitempty"`
	Name              string  `json:"name,omitempty"`
	Status            string  `json:"status,omitempty"`
	Type              string  `json:"type,omitempty"`
	Enabled           bool    `json:"enabled"`
	FailureThreshold  float64 `json:"failure_threshold"`
	RecoveryThreshold float64 `json:"recovery_threshold"`
}
type DetectorPage struct {
	Detectors  []Detector `json:"detectors"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

type projectReference struct{ ID string }

type providerPayloadError struct{ cause error }

func (e providerPayloadError) Error() string { return ErrUnavailable.Error() }
func (e providerPayloadError) Unwrap() []error {
	return []error{ErrUnavailable, e.cause}
}

func (p *projectReference) UnmarshalJSON(raw []byte) error {
	var object struct {
		ID json.RawMessage `json:"id"`
	}
	if len(raw) > 0 && raw[0] == '{' {
		if err := json.Unmarshal(raw, &object); err != nil {
			return providerPayloadError{cause: err}
		}
		raw = object.ID
	}
	var text string
	textErr := json.Unmarshal(raw, &text)
	if textErr == nil {
		p.ID = text
		return nil
	}
	var number json.Number
	numberErr := json.Unmarshal(raw, &number)
	if numberErr == nil {
		p.ID = number.String()
		return nil
	}
	return providerPayloadError{cause: errors.Join(textErr, numberErr)}
}

type sentryMonitor struct {
	Slug, Name, Status, Type string
	Project                  projectReference
	LastCheckIn, NextCheckIn time.Time
	Environments             []sentryMonitorEnvironment
	Config                   struct {
		CheckInMargin         int64 `json:"checkin_margin"`
		MaxRuntime            int64 `json:"max_runtime"`
		FailureIssueThreshold int64 `json:"failure_issue_threshold"`
		RecoveryThreshold     int64 `json:"recovery_threshold"`
	}
}
type sentryMonitorEnvironment struct {
	Name, Status             string
	LastCheckIn, NextCheckIn time.Time
	NextCheckInLatest        time.Time
	ActiveIncident           *sentryMonitorIncident
}
type sentryMonitorIncident struct {
	StartingTimestamp  time.Time  `json:"startingTimestamp"`
	ResolvingTimestamp *time.Time `json:"resolvingTimestamp"`
}

func (e *sentryMonitorEnvironment) UnmarshalJSON(raw []byte) error {
	var name string
	if json.Unmarshal(raw, &name) == nil {
		e.Name = name
		return nil
	}
	var object struct {
		Name, Status      string
		LastCheckIn       json.RawMessage        `json:"lastCheckIn"`
		NextCheckIn       time.Time              `json:"nextCheckIn"`
		NextCheckInLatest time.Time              `json:"nextCheckInLatest"`
		ActiveIncident    *sentryMonitorIncident `json:"activeIncident"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return providerPayloadError{cause: err}
	}
	e.Name, e.Status, e.NextCheckIn = object.Name, object.Status, object.NextCheckIn
	e.NextCheckInLatest, e.ActiveIncident = object.NextCheckInLatest, object.ActiveIncident
	if len(object.LastCheckIn) > 0 && string(object.LastCheckIn) != "null" {
		if err := json.Unmarshal(object.LastCheckIn, &e.LastCheckIn); err != nil {
			var check struct {
				DateCreated time.Time `json:"dateCreated"`
			}
			if fallbackErr := json.Unmarshal(object.LastCheckIn, &check); fallbackErr != nil {
				return providerPayloadError{cause: errors.Join(err, fallbackErr)}
			}
			e.LastCheckIn = check.DateCreated
		}
	}
	return nil
}

type sentryCheckIn struct {
	ID, Status, Environment                             string
	DateCreated, DateAdded, DateUpdated, DateInProgress time.Time
	Duration                                            int64
}
type sentryDetector struct {
	ID, Name, Status, Type string
	Enabled                bool
	IsEnabled              bool `json:"isEnabled"`
	Project                projectReference
	ProjectID              projectReference `json:"projectId"`
	DataSource             struct {
		QueryType string `json:"queryType"`
	}
	LatestGroup    struct{ ID, Status string }
	ConditionGroup struct {
		Conditions []struct {
			Type       string
			Comparison float64
		}
	} `json:"conditionGroup"`
}

func (s *Service) selectedProject(ctx context.Context, slug string) (sentryProject, error) {
	if !safeToken.MatchString(slug) {
		return sentryProject{}, ErrInvalid
	}
	allowed := false
	for _, value := range s.projects {
		if value == slug {
			allowed = true
			break
		}
	}
	if !allowed {
		return sentryProject{}, ErrForbidden
	}
	var project sentryProject
	if _, err := s.get(ctx, "/api/0/projects/"+url.PathEscape(s.org)+"/"+url.PathEscape(slug)+"/", nil, &project); err != nil {
		return project, err
	}
	if project.Slug != slug {
		return sentryProject{}, ErrUnavailable
	}
	if _, err := strconv.ParseUint(project.ID, 10, 64); err != nil {
		return sentryProject{}, ErrUnavailable
	}
	return project, nil
}
func safeMonitor(v sentryMonitor, project sentryProject) (Monitor, bool) {
	if v.Project.ID != project.ID {
		return Monitor{}, false
	}
	slug := boundedToken(v.Slug, 128)
	if slug == "" {
		return Monitor{}, false
	}
	if !monitorStatuses[v.Status] || !monitorTypes[v.Type] || !validOptionalProviderTime(v.LastCheckIn) || !validOptionalProviderTime(v.NextCheckIn) || !validMonitorConfig(v.Config.CheckInMargin, v.Config.MaxRuntime, v.Config.FailureIssueThreshold, v.Config.RecoveryThreshold) || len(v.Environments) > 16 {
		return Monitor{}, false
	}
	out := Monitor{Project: project.Slug, Slug: slug, Name: safeDisplay(v.Name), Status: v.Status, Type: v.Type, Enabled: v.Status == "active", LastCheckIn: v.LastCheckIn, NextExpected: v.NextCheckIn, CheckInMargin: v.Config.CheckInMargin, MaxRuntime: v.Config.MaxRuntime, FailureThreshold: v.Config.FailureIssueThreshold, RecoveryThreshold: v.Config.RecoveryThreshold, Environments: []MonitorEnvironment{}}
	for _, environment := range v.Environments {
		e := boundedToken(environment.Name, 64)
		if e == "" || !monitorEnvironmentStatuses[environment.Status] || !validOptionalProviderTime(environment.LastCheckIn) || !validOptionalProviderTime(environment.NextCheckIn) || !validOptionalProviderTime(environment.NextCheckInLatest) {
			return Monitor{}, false
		}
		projected := MonitorEnvironment{Name: e, Status: environment.Status, LastCheckIn: environment.LastCheckIn, NextExpected: environment.NextCheckIn, NextLatest: environment.NextCheckInLatest}
		if environment.ActiveIncident != nil {
			if !validProviderTime(environment.ActiveIncident.StartingTimestamp) || environment.ActiveIncident.ResolvingTimestamp != nil && !validProviderTime(*environment.ActiveIncident.ResolvingTimestamp) {
				return Monitor{}, false
			}
			projected.Incident = &MonitorIncident{StartedAt: environment.ActiveIncident.StartingTimestamp, ResolvingAt: environment.ActiveIncident.ResolvingTimestamp}
		}
		out.Environments = append(out.Environments, projected)
	}
	return out, true
}

var monitorStatuses = map[string]bool{"active": true, "disabled": true}
var monitorTypes = map[string]bool{"": true, "cron_job": true}
var monitorEnvironmentStatuses = map[string]bool{"active": true, "ok": true, "error": true, "missed_checkin": true, "timeout": true, "in_progress": true, "disabled": true}
var checkInStatuses = map[string]bool{"ok": true, "error": true, "missed": true, "timeout": true, "in_progress": true}

func validMonitorConfig(margin, runtime, failure, recovery int64) bool {
	return margin >= 0 && margin <= 1440 && runtime >= 1 && runtime <= 1440 && failure >= 1 && failure <= 100 && recovery >= 1 && recovery <= 100
}

func validOptionalProviderTime(v time.Time) bool { return v.IsZero() || validProviderTime(v) }

func safeDetector(v sentryDetector, project sentryProject) (Detector, bool) {
	projectID := v.Project.ID
	if projectID == "" {
		projectID = v.ProjectID.ID
	}
	id := boundedToken(v.ID, 128)
	if projectID != project.ID || id == "" {
		return Detector{}, false
	}
	status := v.Status
	if status == "" {
		status = v.LatestGroup.Status
	}
	typ := v.Type
	if typ == "" {
		typ = v.DataSource.QueryType
	}
	out := Detector{Project: project.Slug, ID: id, IssueID: boundedToken(v.LatestGroup.ID, 128), Name: safeDisplay(v.Name), Status: boundedToken(status, 32), Type: boundedToken(typ, 32), Enabled: v.Enabled || v.IsEnabled}
	if v.LatestGroup.ID != "" && out.IssueID == "" || status != "" && out.Status == "" || typ != "" && out.Type == "" {
		return Detector{}, false
	}
	for _, condition := range v.ConditionGroup.Conditions {
		if math.IsNaN(condition.Comparison) || math.IsInf(condition.Comparison, 0) || math.Abs(condition.Comparison) > 1e12 {
			return Detector{}, false
		}
		switch condition.Type {
		case "gt":
			out.FailureThreshold = condition.Comparison
		case "lte":
			out.RecoveryThreshold = condition.Comparison
		}
	}
	return out, true
}
func safeDisplay(v string) string {
	v = bounded(v, 128)
	for _, r := range v {
		if !(r == ' ' || r == '-' || r == '_' || r == '.' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return ""
		}
	}
	return v
}
func safeCount(v int64) int64 {
	if v < 0 || v > 86400*365 {
		return 0
	}
	return v
}
func safeCheckIn(v sentryCheckIn) (CheckIn, bool) {
	id, status, environment := boundedToken(v.ID, 128), boundedToken(v.Status, 32), boundedToken(v.Environment, 64)
	if id == "" || !checkInStatuses[status] || !validOptionalProviderTime(v.DateUpdated) || !validOptionalProviderTime(v.DateAdded) || !validOptionalProviderTime(v.DateInProgress) {
		return CheckIn{}, false
	}
	finished := v.DateUpdated
	if finished.IsZero() {
		finished = v.DateAdded
	}
	return CheckIn{ID: id, Status: status, StartedAt: v.DateInProgress, FinishedAt: finished, DurationMS: safeCount(v.Duration), Environment: environment}, true
}

func (s *Service) ListMonitors(ctx context.Context, actor auth.User, projectSlug, cursor string, limit int) (MonitorPage, error) {
	out := MonitorPage{Monitors: []Monitor{}}
	if !validCursor(cursor) || limit < 1 || limit > 100 {
		return out, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := s.preAuthorize(ctx, actor); err != nil {
		return out, err
	}
	project, err := s.selectedProject(ctx, projectSlug)
	if err != nil {
		return out, err
	}
	q := url.Values{"project": {project.ID}, "per_page": {strconv.Itoa(limit)}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var raw []sentryMonitor
	header, err := s.get(ctx, "/api/0/organizations/"+url.PathEscape(s.org)+"/monitors/", q, &raw)
	if err != nil {
		return out, err
	}
	if len(raw) > limit {
		raw = raw[:limit]
	}
	for _, v := range raw {
		if safe, ok := safeMonitor(v, project); ok {
			safe.CheckInMargin = safeCount(safe.CheckInMargin)
			safe.MaxRuntime = safeCount(safe.MaxRuntime)
			safe.FailureThreshold = safeCount(safe.FailureThreshold)
			safe.RecoveryThreshold = safeCount(safe.RecoveryThreshold)
			out.Monitors = append(out.Monitors, safe)
		} else {
			return MonitorPage{}, ErrUnavailable
		}
	}
	out.NextCursor = nextCursor(header.Get("Link"))
	if err = s.auditRemote(ctx, actor, "sentry_monitors", projectSlug, len(out.Monitors)); err != nil {
		return MonitorPage{}, err
	}
	return out, nil
}
func (s *Service) ListMonitorCheckIns(ctx context.Context, actor auth.User, projectSlug, slug, cursor string, limit int) (CheckInPage, error) {
	out := CheckInPage{CheckIns: []CheckIn{}}
	if !safeToken.MatchString(slug) || !validCursor(cursor) || limit < 1 || limit > 100 {
		return out, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := s.preAuthorize(ctx, actor); err != nil {
		return out, err
	}
	project, err := s.selectedProject(ctx, projectSlug)
	if err != nil {
		return out, err
	}
	q := url.Values{"per_page": {strconv.Itoa(limit)}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var raw []sentryCheckIn
	header, err := s.get(ctx, "/api/0/projects/"+url.PathEscape(s.org)+"/"+url.PathEscape(project.Slug)+"/monitors/"+url.PathEscape(slug)+"/checkins/", q, &raw)
	if err != nil {
		return out, err
	}
	if len(raw) > limit {
		raw = raw[:limit]
	}
	for _, v := range raw {
		checkIn, ok := safeCheckIn(v)
		if !ok {
			return CheckInPage{}, ErrUnavailable
		}
		out.CheckIns = append(out.CheckIns, checkIn)
	}
	out.NextCursor = nextCursor(header.Get("Link"))
	if err = s.auditRemote(ctx, actor, "sentry_monitor_checkins", slug, len(out.CheckIns)); err != nil {
		return CheckInPage{}, err
	}
	return out, nil
}
func (s *Service) ListDetectors(ctx context.Context, actor auth.User, projectSlug, cursor string, limit int) (DetectorPage, error) {
	out := DetectorPage{Detectors: []Detector{}}
	if !validCursor(cursor) || limit < 1 || limit > 100 {
		return out, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := s.preAuthorize(ctx, actor); err != nil {
		return out, err
	}
	project, err := s.selectedProject(ctx, projectSlug)
	if err != nil {
		return out, err
	}
	q := url.Values{"project": {project.ID}, "per_page": {strconv.Itoa(limit)}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var raw []sentryDetector
	header, err := s.get(ctx, "/api/0/organizations/"+url.PathEscape(s.org)+"/detectors/", q, &raw)
	if err != nil {
		return out, err
	}
	if len(raw) > limit {
		raw = raw[:limit]
	}
	for _, v := range raw {
		detector, ok := safeDetector(v, project)
		if !ok {
			return DetectorPage{}, ErrUnavailable
		}
		out.Detectors = append(out.Detectors, detector)
	}
	out.NextCursor = nextCursor(header.Get("Link"))
	if err = s.auditRemote(ctx, actor, "sentry_detectors", projectSlug, len(out.Detectors)); err != nil {
		return DetectorPage{}, err
	}
	return out, nil
}
