// Package posthogquery reads aggregate product analytics for operators from PostHog's Query API. The personal API
// key and project id stay on the server; callers choose only a day range from a fixed list, never HogQL, and get
// aggregates only (no distinct ids). Results are cached so several operators share one upstream call.
package posthogquery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotConfigured = errors.New("posthog analytics is not configured")
	ErrInvalid       = errors.New("invalid analytics request")
	// ErrProvider wraps every upstream failure; the HTTP layer maps it to one safe 503 and keeps the cause for the log.
	ErrProvider = errors.New("posthog query failed")
)

const (
	cacheTTL       = 10 * time.Minute
	requestTimeout = 20 * time.Second
	responseLimit  = 1 << 20
	DefaultDays    = 30
)

// Days are the ranges an operator may ask for; anything else is refused so every query shape is known in advance.
var Days = []int{7, 30, 90}

type Config struct {
	Host      string // https origin of the PostHog instance, from server configuration
	ProjectID string
	Key       string // personal API key with query:read only
	Client    *http.Client
	Now       func() time.Time
}

type Service struct {
	cfg    Config
	client *http.Client
	now    func() time.Time
	mu     sync.Mutex
	cache  map[int]cached
	locks  map[int]*sync.Mutex
}

type cached struct {
	at    time.Time
	value Overview
}

func New(cfg Config) *Service {
	s := &Service{cfg: cfg, client: cfg.Client, now: cfg.Now, cache: map[int]cached{}, locks: map[int]*sync.Mutex{}}
	if s.now == nil {
		s.now = time.Now
	}
	if s.client == nil {
		s.client = &http.Client{}
	}
	// The bearer key must never follow a redirect to another origin.
	c := *s.client
	c.Timeout = requestTimeout
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	s.client = &c
	return s
}

type Status struct {
	Configured bool   `json:"configured"`
	Host       string `json:"host,omitempty"`
}

func (s *Service) Status() Status {
	if s == nil || !s.configured() {
		return Status{}
	}
	return Status{Configured: true, Host: s.cfg.Host}
}

func (s *Service) configured() bool {
	return s.cfg.Host != "" && s.cfg.ProjectID != "" && s.cfg.Key != ""
}

type Totals struct {
	Visitors          int64 `json:"visitors"`
	PageViews         int64 `json:"page_views"`
	ConsoleUsers      int64 `json:"console_users"`
	SignupClicks      int64 `json:"signup_clicks"`
	AddMachineStarted int64 `json:"add_machine_started"`
	MachinesPaired    int64 `json:"machines_paired"`
	Checkouts         int64 `json:"checkouts"`
}

type Day struct {
	Day       string `json:"day"`
	PageViews int64  `json:"page_views"`
	Visitors  int64  `json:"visitors"`
}

type Count struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

// Cohort is the accounts whose first machine pairing fell in one week, and how many of them were active (sent
// console_active_day) 0, 1, 2… weeks later. Active is empty until the console sends that event.
type Cohort struct {
	Week   string  `json:"week"`
	Size   int64   `json:"size"`
	Active []int64 `json:"active"`
}

type FunnelStep struct {
	Step  string `json:"step"`
	Count int64  `json:"count"`
}

// Overview carries one entry per insight. An insight that failed is named in Errors and left empty, so one slow
// query never blanks the whole page. Visitors are marketing-site visits; console users are accounts: the two id
// spaces are disjoint by design and are never added together.
type Overview struct {
	ObservedAt time.Time         `json:"observed_at"`
	Days       int               `json:"days"`
	Totals     Totals            `json:"totals"`
	Daily      []Day             `json:"daily"`
	TopPages   []Count           `json:"top_pages"`
	Referrers  []Count           `json:"referrers"`
	Countries  []Count           `json:"countries"`
	Devices    []Count           `json:"devices"`
	Funnel     []FunnelStep      `json:"funnel"`
	Retention  []Cohort          `json:"retention"`
	Errors     map[string]string `json:"errors,omitempty"`
}

const consoleEvents = `'add_machine_started','machine_paired','cli_approved','preview_created','invitation_accepted','checkout_completed'`

func since(days int) string { return fmt.Sprintf("timestamp >= now() - INTERVAL %d DAY", days) }

func totalsQuery(days int) string {
	return `SELECT uniqIf(distinct_id, event='pageview'), countIf(event='pageview'),
 uniqIf(distinct_id, event IN (` + consoleEvents + `)), countIf(event='signup_clicked'),
 uniqIf(distinct_id, event='add_machine_started'), uniqIf(distinct_id, event='machine_paired'),
 uniqIf(distinct_id, event='checkout_completed') FROM events WHERE ` + since(days)
}

func dailyQuery(days int) string {
	return `SELECT toString(toDate(timestamp)) AS day, countIf(event='pageview'), uniqIf(distinct_id, event='pageview')
 FROM events WHERE ` + since(days) + ` GROUP BY day ORDER BY day`
}

// Weekly cohorts by first machine pairing, and the same cohorts' activity weeks later (console_active_day).
func cohortSizesQuery(days int) string {
	return `SELECT toString(cohort), count() FROM (SELECT distinct_id, toStartOfWeek(min(timestamp)) AS cohort FROM events WHERE event='machine_paired' GROUP BY distinct_id)
 WHERE cohort >= now() - INTERVAL ` + fmt.Sprint(days) + ` DAY GROUP BY cohort ORDER BY cohort`
}

func cohortActivityQuery(days int) string {
	return `SELECT toString(c.cohort), dateDiff('week', c.cohort, toStartOfWeek(a.timestamp)) AS week, uniq(a.distinct_id)
 FROM events AS a INNER JOIN (SELECT distinct_id, toStartOfWeek(min(timestamp)) AS cohort FROM events WHERE event='machine_paired' GROUP BY distinct_id) AS c ON a.distinct_id = c.distinct_id
 WHERE a.event='console_active_day' AND c.cohort >= now() - INTERVAL ` + fmt.Sprint(days) + ` DAY GROUP BY c.cohort, week ORDER BY c.cohort, week`
}

func topQuery(days int, expr string) string {
	return `SELECT coalesce(nullIf(toString(` + expr + `),''),'(none)') AS name, count() AS n FROM events
 WHERE event='pageview' AND ` + since(days) + ` GROUP BY name ORDER BY n DESC LIMIT 20`
}

// Overview returns the insights for the last `days` days (0 means DefaultDays).
func (s *Service) Overview(ctx context.Context, days int) (Overview, error) {
	if s == nil || !s.configured() {
		return Overview{}, ErrNotConfigured
	}
	if days == 0 {
		days = DefaultDays
	}
	if !allowedDays(days) {
		return Overview{}, ErrInvalid
	}
	s.mu.Lock()
	lock := s.locks[days]
	if lock == nil {
		lock = &sync.Mutex{}
		s.locks[days] = lock
	}
	s.mu.Unlock()
	lock.Lock() // one upstream fetch per range; waiters read the fresh cache entry
	defer lock.Unlock()
	s.mu.Lock()
	hit, ok := s.cache[days]
	s.mu.Unlock()
	if ok && s.now().Sub(hit.at) < cacheTTL {
		return hit.value, nil
	}
	out, err := s.fetch(ctx, days)
	if err != nil {
		return Overview{}, err
	}
	s.mu.Lock()
	s.cache[days] = cached{at: out.ObservedAt, value: out}
	s.mu.Unlock()
	return out, nil
}

func allowedDays(days int) bool {
	for _, d := range Days {
		if d == days {
			return true
		}
	}
	return false
}

func (s *Service) fetch(ctx context.Context, days int) (Overview, error) {
	out := Overview{ObservedAt: s.now().UTC(), Days: days, Daily: []Day{}, TopPages: []Count{}, Referrers: []Count{}, Countries: []Count{}, Devices: []Count{}, Funnel: []FunnelStep{}, Retention: []Cohort{}}
	type job struct {
		name  string
		query string
		apply func([][]any) error
	}
	counts := func(dst *[]Count) func([][]any) error {
		return func(rows [][]any) error {
			for _, r := range rows {
				if len(r) != 2 {
					return errors.New("unexpected column count")
				}
				*dst = append(*dst, Count{Name: fmt.Sprint(r[0]), Count: toInt(r[1])})
			}
			return nil
		}
	}
	var activity [][3]any
	jobs := []job{
		{"totals", totalsQuery(days), func(rows [][]any) error {
			if len(rows) != 1 || len(rows[0]) != 7 {
				return errors.New("unexpected totals shape")
			}
			r := rows[0]
			out.Totals = Totals{toInt(r[0]), toInt(r[1]), toInt(r[2]), toInt(r[3]), toInt(r[4]), toInt(r[5]), toInt(r[6])}
			return nil
		}},
		{"daily", dailyQuery(days), func(rows [][]any) error {
			for _, r := range rows {
				if len(r) != 3 {
					return errors.New("unexpected column count")
				}
				out.Daily = append(out.Daily, Day{Day: fmt.Sprint(r[0]), PageViews: toInt(r[1]), Visitors: toInt(r[2])})
			}
			return nil
		}},
		{"top_pages", topQuery(days, "properties.path"), counts(&out.TopPages)},
		{"referrers", topQuery(days, "properties.referrer_host"), counts(&out.Referrers)},
		{"countries", topQuery(days, "properties.$geoip_country_code"), counts(&out.Countries)},
		{"devices", topQuery(days, "properties.$device_type"), counts(&out.Devices)},
		{"retention_cohorts", cohortSizesQuery(days), func(rows [][]any) error {
			for _, r := range rows {
				if len(r) != 2 {
					return errors.New("unexpected column count")
				}
				out.Retention = append(out.Retention, Cohort{Week: fmt.Sprint(r[0]), Size: toInt(r[1]), Active: []int64{}})
			}
			return nil
		}},
		{"retention", cohortActivityQuery(days), func(rows [][]any) error {
			for _, r := range rows {
				if len(r) != 3 {
					return errors.New("unexpected column count")
				}
				activity = append(activity, [3]any{r[0], r[1], r[2]})
			}
			return nil
		}},
	}
	rowsByJob := make([][][]any, len(jobs))
	errsByJob := make([]error, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rowsByJob[i], errsByJob[i] = s.query(ctx, j.query)
		}()
	}
	wg.Wait()
	failed := 0
	for i, j := range jobs {
		err := errsByJob[i]
		if err == nil {
			err = j.apply(rowsByJob[i])
		}
		if err != nil {
			failed++
			if out.Errors == nil {
				out.Errors = map[string]string{}
			}
			out.Errors[j.name] = errorCode(err)
			if ctx.Err() != nil {
				return Overview{}, fmt.Errorf("%w: %w", ErrProvider, ctx.Err())
			}
		}
	}
	for _, row := range activity {
		for i := range out.Retention {
			week := int(toInt(row[1]))
			if out.Retention[i].Week == fmt.Sprint(row[0]) && week >= 0 && week < 53 {
				for len(out.Retention[i].Active) <= week {
					out.Retention[i].Active = append(out.Retention[i].Active, 0)
				}
				out.Retention[i].Active[week] = toInt(row[2])
			}
		}
	}
	if failed == len(jobs) {
		return Overview{}, fmt.Errorf("%w: every insight failed: %w", ErrProvider, firstError(errsByJob))
	}
	out.Funnel = []FunnelStep{{"Site visits", out.Totals.Visitors}, {"Sign-up clicks", out.Totals.SignupClicks},
		{"Add-machine started", out.Totals.AddMachineStarted}, {"Machine paired", out.Totals.MachinesPaired}}
	return out, nil
}

func firstError(errs []error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// errorCode is the short, safe reason shown to operators; the full cause stays in the server log.
func errorCode(err error) string {
	var he *httpError
	if errors.As(err, &he) {
		switch {
		case he.status == 401 || he.status == 403:
			return "unauthorized"
		case he.status == 429:
			return "rate_limited"
		case he.status >= 500:
			return "provider_unavailable"
		}
		return "rejected"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "unexpected_response"
}

type httpError struct{ status int }

func (e *httpError) Error() string { return fmt.Sprintf("posthog answered HTTP %d", e.status) }

func (s *Service) query(ctx context.Context, hogql string) ([][]any, error) {
	body, err := json.Marshal(map[string]any{"query": map[string]string{"kind": "HogQLQuery", "query": hogql}})
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(s.cfg.Host, "/") + "/api/projects/" + url.PathEscape(s.cfg.ProjectID) + "/query/"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, errors.New("posthog request failed: " + redactURLError(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, responseLimit+1))
	if err != nil || len(raw) > responseLimit {
		return nil, errors.New("posthog response unreadable or too large")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &httpError{status: resp.StatusCode}
	}
	var decoded struct {
		Results [][]any `json:"results"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, errors.New("posthog response is not JSON results")
	}
	return decoded.Results, nil
}

// redactURLError drops the request URL that net/http puts in its errors.
func redactURLError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

func toInt(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		var i int64
		fmt.Sscan(n, &i)
		return i
	}
	return 0
}
