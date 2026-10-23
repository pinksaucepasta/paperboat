// Package operatorreporting provides the bounded, audited operator view of
// locally recorded support references and their corresponding Sentry issues.
package operatorreporting

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

const maxResponseBytes = 1 << 20
const requestTimeout = 5 * time.Second

var (
	ErrInvalid          = errors.New("invalid operator reporting request")
	ErrForbidden        = errors.New("operator reporting read forbidden")
	ErrDisabled         = errors.New("operator reporting is disabled")
	ErrNotFound         = errors.New("operator reporting resource not found")
	ErrRateLimited      = errors.New("operator reporting rate limited")
	ErrUnavailable      = errors.New("operator reporting unavailable")
	errProviderRejected = errors.New("operator reporting provider rejected fixed query")
)

var safeToken = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

type Config struct {
	BaseURL      string
	Organization string
	Projects     []string
	Token        string
}

type Status struct {
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason,omitempty"`
}

type Service struct {
	store    *db.DB
	audit    *audit.Writer
	baseURL  *url.URL
	org      string
	projects []string
	token    string
	client   *http.Client
}

func New(store *db.DB, writer *audit.Writer, cfg Config) (*Service, error) {
	if store == nil || store.Pool() == nil {
		return nil, ErrInvalid
	}
	s := &Service{store: store, audit: writer}
	if s.audit == nil {
		s.audit = audit.NewWriter(store)
	}
	if cfg.BaseURL == "" && cfg.Organization == "" && len(cfg.Projects) == 0 && cfg.Token == "" {
		return s, nil
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://sentry.io"
	}
	u, err := url.Parse(cfg.BaseURL)
	seen := map[string]bool{}
	projectsOK := len(cfg.Projects) >= 1 && len(cfg.Projects) <= 8
	for _, project := range cfg.Projects {
		if !safeToken.MatchString(project) || len(project) > 64 || seen[project] {
			projectsOK = false
		}
		seen[project] = true
	}
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || !safeToken.MatchString(cfg.Organization) || !projectsOK || strings.TrimSpace(cfg.Token) == "" || len(cfg.Token) > 4096 {
		return nil, ErrInvalid
	}
	u.Path = ""
	s.baseURL, s.org, s.projects, s.token = u, cfg.Organization, append([]string(nil), cfg.Projects...), cfg.Token
	s.client = &http.Client{
		Timeout:       3 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	return s, nil
}

func (s *Service) Status() Status {
	if s == nil || s.baseURL == nil {
		return Status{Reason: "unconfigured"}
	}
	return Status{Enabled: true}
}

type LocalReference struct {
	Reference string    `json:"support_reference"`
	OwnerID   string    `json:"owner_id"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Reports   int       `json:"reports"`
	EventID   string    `json:"sentry_event_id,omitempty"`
	Reporting string    `json:"reporting"`
}
type LocalReferencePage struct {
	References []LocalReference `json:"references"`
	NextAfter  string           `json:"next_after,omitempty"`
}

type Issue struct {
	Project   string    `json:"project"`
	ID        string    `json:"id"`
	ShortID   string    `json:"short_id,omitempty"`
	Level     string    `json:"level,omitempty"`
	Status    string    `json:"status"`
	Count     int64     `json:"count"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}
type IssuePage struct {
	Issues     []Issue `json:"issues"`
	NextCursor string  `json:"next_cursor,omitempty"`
}
type Event struct {
	Region           string    `json:"region,omitempty"`
	Instance         string    `json:"instance,omitempty"`
	Project          string    `json:"project"`
	ID               string    `json:"id"`
	CreatedAt        time.Time `json:"created_at"`
	Platform         string    `json:"platform,omitempty"`
	SupportReference string    `json:"support_reference,omitempty"`
	Component        string    `json:"component,omitempty"`
	Stage            string    `json:"stage,omitempty"`
	Code             string    `json:"code,omitempty"`
}
type IssueDetail struct {
	Issue  Issue   `json:"issue"`
	Events []Event `json:"events"`
}
type ReferenceResolution struct {
	Reference string         `json:"support_reference"`
	Local     LocalReference `json:"local"`
	Issue     Issue          `json:"issue"`
	Event     Event          `json:"event"`
}

type localCursor struct {
	Reference string `json:"reference"`
	OwnerID   string `json:"owner_id"`
}

func (s *Service) ListLocalReferences(ctx context.Context, actor auth.User, after string, limit int) (LocalReferencePage, error) {
	var page LocalReferencePage
	cursor, ok := decodeLocalCursor(after)
	if s == nil || s.store == nil || !ok || limit < 1 || limit > 100 {
		return page, ErrInvalid
	}
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		page = LocalReferencePage{References: []LocalReference{}}
		if err := authorize(ctx, tx, actor.ID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT support_reference,owner_id,min(created_at),max(created_at),count(*),coalesce((array_agg(sentry_event_id ORDER BY created_at DESC,id DESC) FILTER (WHERE sentry_event_id<>''))[1],''),CASE WHEN bool_or(reporting='unconfirmed') THEN 'unconfirmed' WHEN bool_or(reporting='filtered') THEN 'filtered' ELSE 'disabled' END FROM support_reports WHERE expires_at>now() AND (support_reference,owner_id)>($1,$2) GROUP BY support_reference,owner_id ORDER BY support_reference,owner_id LIMIT $3`, cursor.Reference, cursor.OwnerID, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v LocalReference
			if err := rows.Scan(&v.Reference, &v.OwnerID, &v.FirstSeen, &v.LastSeen, &v.Reports, &v.EventID, &v.Reporting); err != nil {
				return err
			}
			if len(page.References) < limit {
				page.References = append(page.References, v)
			} else {
				last := page.References[len(page.References)-1]
				page.NextAfter = encodeLocalCursor(localCursor{Reference: last.Reference, OwnerID: last.OwnerID})
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actor.ID, ActorType: audit.ActorAdmin, EventType: "operator_reporting.read", ResourceType: "support_references", ResourceID: after, Metadata: map[string]any{"outcome": "available", "count": len(page.References)}})
	})
	if err != nil {
		return LocalReferencePage{}, classifyDB(err)
	}
	return page, nil
}

func (s *Service) ListIssues(ctx context.Context, actor auth.User, cursor string, limit int) (IssuePage, error) {
	page := IssuePage{Issues: []Issue{}}
	if !validCursor(cursor) || limit < 1 || limit > 100 {
		return page, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := s.preAuthorize(ctx, actor); err != nil {
		return page, err
	}
	projects, err := s.projectDetails(ctx)
	if err != nil {
		return page, err
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}, "collapse": {"stats"}}
	for _, p := range projects {
		q.Add("project", p.ID)
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var raw []sentryIssue
	header, err := s.get(ctx, "/api/0/organizations/"+url.PathEscape(s.org)+"/issues/", q, &raw)
	if err != nil {
		return page, err
	}
	if len(raw) > limit {
		raw = raw[:limit]
	}
	for _, v := range raw {
		if !v.inProjects(projects) {
			return IssuePage{}, ErrUnavailable
		}
		page.Issues = append(page.Issues, v.safe())
	}
	page.NextCursor = nextCursor(header.Get("Link"))
	if err := s.auditRemote(ctx, actor, "sentry_issues", strings.Join(s.projects, ","), len(page.Issues)); err != nil {
		return IssuePage{}, err
	}
	return page, nil
}

func (s *Service) GetIssue(ctx context.Context, actor auth.User, issueID string, eventLimit int) (IssueDetail, error) {
	out := IssueDetail{Events: []Event{}}
	if !safeToken.MatchString(issueID) || eventLimit < 1 || eventLimit > 10 {
		return out, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := s.preAuthorize(ctx, actor); err != nil {
		return out, err
	}
	projects, err := s.projectDetails(ctx)
	if err != nil {
		return out, err
	}
	var issue sentryIssue
	if _, err := s.get(ctx, "/api/0/organizations/"+url.PathEscape(s.org)+"/issues/"+url.PathEscape(issueID)+"/", nil, &issue); err != nil {
		return out, err
	}
	if !issue.inProjects(projects) {
		return out, ErrNotFound
	}
	var events []sentryEvent
	q := url.Values{"per_page": {strconv.Itoa(eventLimit)}, "full": {"false"}}
	if _, err := s.get(ctx, "/api/0/organizations/"+url.PathEscape(s.org)+"/issues/"+url.PathEscape(issueID)+"/events/", q, &events); err != nil {
		return out, err
	}
	out.Issue = issue.safe()
	if len(events) > eventLimit {
		events = events[:eventLimit]
	}
	for _, v := range events {
		e := v.safe()
		e.Project = issue.Project.Slug
		out.Events = append(out.Events, e)
	}
	if err := s.auditRemote(ctx, actor, "sentry_issue", issueID, len(out.Events)); err != nil {
		return IssueDetail{}, err
	}
	return out, nil
}

func (s *Service) ResolveReference(ctx context.Context, actor auth.User, reference, owner string) (ReferenceResolution, error) {
	var out ReferenceResolution
	if !supportreport.ValidReference(reference) || len(owner) > 128 || (owner != "" && !safeToken.MatchString(owner)) {
		return out, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := s.preAuthorize(ctx, actor); err != nil {
		return out, err
	}
	var local LocalReference
	var found bool
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := authorize(ctx, tx, actor.ID); err != nil {
			return err
		}
		local = LocalReference{}
		found = false
		err := tx.QueryRow(ctx, `SELECT support_reference,owner_id,min(created_at),max(created_at),count(*),coalesce((array_agg(sentry_event_id ORDER BY created_at DESC,id DESC) FILTER (WHERE sentry_event_id<>''))[1],''),CASE WHEN bool_or(reporting='unconfirmed') THEN 'unconfirmed' WHEN bool_or(reporting='filtered') THEN 'filtered' ELSE 'disabled' END FROM support_reports WHERE expires_at>now() AND support_reference=$1 AND owner_id=$2 GROUP BY support_reference,owner_id`, reference, owner).Scan(&local.Reference, &local.OwnerID, &local.FirstSeen, &local.LastSeen, &local.Reports, &local.EventID, &local.Reporting)
		if reportingErrorOnly(err, pgx.ErrNoRows) {
			return nil
		}
		if err == nil {
			found = true
		}
		return err
	})
	if err != nil {
		return out, classifyDB(err)
	}
	if !found {
		return out, ErrNotFound
	}
	out.Reference = reference
	out.Local = local
	if out.Local.EventID == "" {
		return out, ErrNotFound
	}
	projects, err := s.projectDetails(ctx)
	if err != nil {
		return ReferenceResolution{}, err
	}
	var raw sentryEvent
	var foundProject sentryProject
	for _, project := range projects {
		_, getErr := s.get(ctx, "/api/0/projects/"+url.PathEscape(s.org)+"/"+url.PathEscape(project.Slug)+"/events/"+url.PathEscape(out.Local.EventID)+"/", nil, &raw)
		if getErr == nil {
			foundProject = project
			break
		}
		if !reportingErrorOnly(getErr, ErrNotFound) {
			return ReferenceResolution{}, getErr
		}
	}
	if foundProject.ID == "" {
		return ReferenceResolution{}, ErrNotFound
	}
	out.Event = raw.safe()
	out.Event.Project = foundProject.Slug
	if out.Event.SupportReference != reference {
		return ReferenceResolution{}, ErrNotFound
	}
	if raw.GroupID == "" {
		return ReferenceResolution{}, ErrNotFound
	}
	var issue sentryIssue
	if _, err = s.get(ctx, "/api/0/organizations/"+url.PathEscape(s.org)+"/issues/"+url.PathEscape(raw.GroupID)+"/", nil, &issue); err != nil {
		return ReferenceResolution{}, err
	}
	out.Issue = issue.safe()
	if !issue.inProjects(projects) || issue.Project.ID != foundProject.ID {
		return ReferenceResolution{}, ErrNotFound
	}
	if err = s.auditRemote(ctx, actor, "support_reference", reference, 1); err != nil {
		return ReferenceResolution{}, err
	}
	return out, nil
}

func validCursor(v string) bool { return len(v) <= 256 && !strings.ContainsAny(v, "\r\n") }

func encodeLocalCursor(v localCursor) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}
func decodeLocalCursor(v string) (localCursor, bool) {
	if v == "" {
		return localCursor{}, true
	}
	if len(v) > 512 {
		return localCursor{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return localCursor{}, false
	}
	var c localCursor
	if json.Unmarshal(b, &c) != nil || !supportreport.ValidReference(c.Reference) || len(c.OwnerID) > 128 || (c.OwnerID != "" && !safeToken.MatchString(c.OwnerID)) {
		return localCursor{}, false
	}
	return c, true
}

func authorize(ctx context.Context, tx *db.Tx, actor string) error {
	if actor == "" {
		return ErrForbidden
	}
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operator_accounts WHERE id=$1 AND status='active' AND role IN ('support','billing','operations','admin'))`, actor).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}
func (s *Service) preAuthorize(ctx context.Context, actor auth.User) error {
	if s == nil || s.store == nil {
		return ErrInvalid
	}
	if s.baseURL == nil {
		return ErrDisabled
	}
	var ok bool
	if err := s.store.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operator_accounts WHERE id=$1 AND status='active' AND role IN ('support','billing','operations','admin'))`, actor.ID).Scan(&ok); err != nil {
		return classifyDB(err)
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}
func (s *Service) auditRemote(ctx context.Context, actor auth.User, kind, id string, count int) error {
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := authorize(ctx, tx, actor.ID); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actor.ID, ActorType: audit.ActorAdmin, EventType: "operator_reporting.read", ResourceType: kind, ResourceID: id, Metadata: map[string]any{"outcome": "available", "count": count}})
	})
	return classifyDB(err)
}
func classifyDB(err error) error {
	if err == nil {
		return nil
	}
	if reportingErrorOnly(err, ErrForbidden) {
		return ErrForbidden
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.Join(ErrUnavailable, err)
}

func (s *Service) get(ctx context.Context, path string, q url.Values, dst any) (http.Header, error) {
	if s == nil || s.baseURL == nil {
		return nil, ErrDisabled
	}
	u := *s.baseURL
	u.Path = path
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.Join(ErrInvalid, err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, errors.Join(ctxErr, err)
		}
		return nil, errors.Join(ErrUnavailable, err)
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	closeErr := resp.Body.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(ErrUnavailable, readErr, closeErr)
	}
	if len(body) > maxResponseBytes {
		return nil, ErrUnavailable
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusBadRequest:
		return nil, errProviderRejected
	case http.StatusNotFound:
		return nil, ErrNotFound
	case http.StatusTooManyRequests:
		return nil, ErrRateLimited
	default:
		return nil, errors.Join(ErrUnavailable, providerStatusError{status: resp.StatusCode})
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	return resp.Header, nil
}

// reportingErrorOnly accepts an expected provider/database outcome only when
// every bounded leaf is one of the recognized sentinels. Mixed, cyclic,
// typed-nil, and truncated failures stay operational.
func reportingErrorOnly(err error, expected ...error) bool {
	ancestors := make(map[error]struct{}, 8)
	visited := 0
	var walk func(error) bool
	walk = func(current error) bool {
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return false
			}
		}
		if value.Comparable() {
			if _, exists := ancestors[current]; exists {
				return false
			}
			ancestors[current] = struct{}{}
			defer delete(ancestors, current)
		}
		visited++
		if visited > 8 {
			return false
		}
		for _, sentinel := range expected {
			if sameReportingError(current, sentinel) {
				return true
			}
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return false
			}
			for _, child := range children {
				if !walk(child) {
					return false
				}
			}
			return true
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			return walk(wrapped.Unwrap())
		}
		return false
	}
	return walk(err)
}

func sameReportingError(current, sentinel error) bool {
	if current == nil || sentinel == nil {
		return current == nil && sentinel == nil
	}
	left, right := reflect.ValueOf(current), reflect.ValueOf(sentinel)
	return left.Type() == right.Type() && left.Comparable() && left.Interface() == right.Interface()
}

type providerStatusError struct{ status int }

func (e providerStatusError) Error() string         { return "provider request failed" }
func (e providerStatusError) DiagnosticStatus() int { return e.status }

type sentryIssue struct {
	ID, ShortID, Title, Culprit, Level, Status, Count string
	FirstSeen, LastSeen                               time.Time
	Project                                           sentryProject
}

type sentryProject struct{ ID, Slug string }

func (v sentryIssue) inProject(p sentryProject) bool {
	return v.Project.ID == p.ID && v.Project.Slug == p.Slug
}
func (v sentryIssue) inProjects(projects []sentryProject) bool {
	for _, p := range projects {
		if v.inProject(p) {
			return true
		}
	}
	return false
}
func (s *Service) projectDetails(ctx context.Context) ([]sentryProject, error) {
	out := make([]sentryProject, 0, len(s.projects))
	ids := map[string]bool{}
	for _, slug := range s.projects {
		var p sentryProject
		_, err := s.get(ctx, "/api/0/projects/"+url.PathEscape(s.org)+"/"+url.PathEscape(slug)+"/", nil, &p)
		if err != nil {
			return nil, err
		}
		if p.ID == "" || p.Slug != slug || ids[p.ID] {
			return nil, ErrUnavailable
		}
		if _, err := strconv.ParseUint(p.ID, 10, 64); err != nil {
			return nil, ErrUnavailable
		}
		ids[p.ID] = true
		out = append(out, p)
	}
	return out, nil
}

func (v sentryIssue) safe() Issue {
	n, _ := strconv.ParseInt(v.Count, 10, 64)
	return Issue{Project: boundedToken(v.Project.Slug, 64), ID: boundedToken(v.ID, 128), ShortID: boundedToken(v.ShortID, 128), Level: boundedToken(v.Level, 32), Status: boundedToken(v.Status, 32), Count: n, FirstSeen: v.FirstSeen, LastSeen: v.LastSeen}
}

type sentryTag struct{ Key, Value string }
type sentryEvent struct {
	ID                        string    `json:"id"`
	EventID                   string    `json:"eventID"`
	GroupID                   string    `json:"groupID"`
	DateCreated               time.Time `json:"dateCreated"`
	Title, Platform, Location string
	Tags                      []sentryTag
}

func (v sentryEvent) safe() Event {
	e := Event{ID: boundedToken(v.EventID, 128), CreatedAt: v.DateCreated, Platform: boundedToken(v.Platform, 32)}
	if e.ID == "" {
		e.ID = boundedToken(v.ID, 128)
	}
	region, instance := "", ""
	for _, t := range v.Tags {
		switch t.Key {
		case "region":
			region = t.Value
		case "instance":
			instance = t.Value
		case "support_reference":
			if supportreport.ValidReference(t.Value) {
				e.SupportReference = t.Value
			}
		case "component":
			if components[t.Value] {
				e.Component = t.Value
			}
		case "stage":
			e.Stage = safeSemantic(t.Value, semanticStages)
		case "code":
			e.Code = safeSemantic(t.Value, semanticCodes)
		}
	}
	e.Region, e.Instance = safeFleet(region, instance)
	return e
}
func bounded(v string, n int) string {
	v = strings.TrimSpace(v)
	if len(v) > n {
		return v[:n]
	}
	return v
}
func boundedToken(v string, n int) string {
	v = bounded(v, n)
	if !safeToken.MatchString(v) {
		return ""
	}
	return v
}
func nextCursor(link string) string {
	for _, part := range strings.Split(link, ",") {
		if !strings.Contains(part, `rel="next"`) || !strings.Contains(part, `results="true"`) {
			continue
		}
		for _, field := range strings.Split(part, ";") {
			field = strings.TrimSpace(field)
			if strings.HasPrefix(field, "cursor=") {
				v := strings.Trim(field[len("cursor="):], `"`)
				if validCursor(v) {
					return v
				}
			}
		}
		start, end := strings.IndexByte(part, '<'), strings.IndexByte(part, '>')
		if start >= 0 && end > start {
			if u, err := url.Parse(part[start+1 : end]); err == nil {
				if v := u.Query().Get("cursor"); validCursor(v) {
					return v
				}
			}
		}
	}
	return ""
}
