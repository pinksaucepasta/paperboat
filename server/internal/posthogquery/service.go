// Package posthogquery reads aggregate product analytics for operators from PostHog's Query API. The personal API
// key and project id stay on the server; callers choose only an app (site or console) and a day range from a fixed
// list, never HogQL, and get aggregates only (no distinct ids). Results are cached so several operators share one
// upstream call.
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
	"regexp"
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
	onlineTTL      = 30 * time.Second
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
	cache  map[string]cached
	locks  map[string]*sync.Mutex
}

type cached struct {
	at    time.Time
	value any
}

func New(cfg Config) *Service {
	s := &Service{cfg: cfg, client: cfg.Client, now: cfg.Now, cache: map[string]cached{}, locks: map[string]*sync.Mutex{}}
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
	// URL opens the project in PostHog (empty when the project is addressed as @current).
	URL string `json:"url,omitempty"`
}

func (s *Service) Status() Status {
	if s == nil || !s.configured() {
		return Status{}
	}
	return Status{Configured: true, Host: s.cfg.Host, URL: s.projectURL("")}
}

// projectURL is the PostHog page for the operator to dig deeper (path is relative to the project).
func (s *Service) projectURL(path string) string {
	if !analyticsProject.MatchString(s.cfg.ProjectID) {
		return ""
	}
	return strings.TrimRight(s.cfg.Host, "/") + "/project/" + s.cfg.ProjectID + path
}

var analyticsProject = regexp.MustCompile(`^[0-9]{1,20}$`)

func (s *Service) configured() bool {
	return s.cfg.Host != "" && s.cfg.ProjectID != "" && s.cfg.Key != ""
}

type job struct {
	name  string
	query string
	apply func([][]any) error
}

// cachedValue returns the fresh cache entry for key or runs fetch once (concurrent callers wait for it).
func (s *Service) cachedValue(key string, ttl time.Duration, fetch func() (any, error)) (any, error) {
	s.mu.Lock()
	lock := s.locks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		s.locks[key] = lock
	}
	s.mu.Unlock()
	lock.Lock() // one upstream fetch per key; waiters read the fresh cache entry
	defer lock.Unlock()
	s.mu.Lock()
	hit, ok := s.cache[key]
	s.mu.Unlock()
	if ok && s.now().Sub(hit.at) < ttl {
		return hit.value, nil
	}
	value, err := fetch()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cache[key] = cached{at: s.now(), value: value}
	s.mu.Unlock()
	return value, nil
}

// runJobs runs the queries in parallel. An insight that failed is named in the returned map and left empty, so one
// slow query never blanks the page; only when every insight failed is the result an error.
func (s *Service) runJobs(ctx context.Context, jobs []job) (map[string]string, error) {
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
	var failures map[string]string
	failed := 0
	for i, j := range jobs {
		err := errsByJob[i]
		if err == nil {
			err = j.apply(rowsByJob[i])
		}
		if err != nil {
			failed++
			if failures == nil {
				failures = map[string]string{}
			}
			failures[j.name] = errorCode(err)
			if ctx.Err() != nil {
				return nil, fmt.Errorf("%w: %w", ErrProvider, ctx.Err())
			}
		}
	}
	if failed == len(jobs) {
		return nil, fmt.Errorf("%w: every insight failed: %w", ErrProvider, firstError(errsByJob))
	}
	return failures, nil
}

func (s *Service) checkRange(days int) (int, error) {
	if s == nil || !s.configured() {
		return 0, ErrNotConfigured
	}
	if days == 0 {
		days = DefaultDays
	}
	if !allowedDays(days) {
		return 0, ErrInvalid
	}
	return days, nil
}

func allowedDays(days int) bool {
	for _, d := range Days {
		if d == days {
			return true
		}
	}
	return false
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
	case bool:
		if n {
			return 1
		}
	}
	return 0
}
