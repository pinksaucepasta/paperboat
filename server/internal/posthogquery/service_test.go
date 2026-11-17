package posthogquery

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakePostHog answers every HogQL query by its shape and records what the server sent.
type fakePostHog struct {
	calls   atomic.Int64
	status  int
	failFor string // a substring of the HogQL that gets HTTP 500
	last    atomic.Value
}

func (f *fakePostHog) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Query struct{ Kind, Query string }
	}
	json.Unmarshal(body, &req)
	f.last.Store(r.Header.Get("Authorization") + "|" + r.URL.Path + "|" + req.Query.Kind)
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	q := req.Query.Query
	if f.failFor != "" && strings.Contains(q, f.failFor) {
		w.WriteHeader(500)
		return
	}
	switch {
	case strings.Contains(q, "uniqIf(distinct_id, event='pageview'), countIf"):
		w.Write([]byte(`{"results":[[12,40,5,6,4,3,1]]}`))
	case strings.Contains(q, "console_active_day"):
		w.Write([]byte(`{"results":[["2026-11-09",0,4],["2026-11-09",1,3],["2026-11-16",0,2]]}`))
	case strings.Contains(q, "toStartOfWeek(min(timestamp))"):
		w.Write([]byte(`{"results":[["2026-11-09",5],["2026-11-16",2]]}`))
	case strings.Contains(q, "toDate(timestamp)"):
		w.Write([]byte(`{"results":[["2026-11-16",10,4],["2026-11-17",30,8]]}`))
	default:
		w.Write([]byte(`{"results":[["/pricing",25],["(none)","7"]]}`))
	}
}

func newTestService(f *fakePostHog, now *time.Time) *Service {
	server := httptest.NewTLSServer(f)
	return New(Config{Host: server.URL, ProjectID: "123", Key: "phx_test_key", Client: server.Client(), Now: func() time.Time { return *now }})
}

func TestOverviewParsesInsightsSendsKeyAndCaches(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	f := &fakePostHog{}
	s := newTestService(f, &now)
	got, err := s.Overview(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Days != 30 || got.Totals.Visitors != 12 || got.Totals.PageViews != 40 || got.Totals.MachinesPaired != 3 || len(got.Daily) != 2 || got.Daily[1].PageViews != 30 {
		t.Fatalf("overview=%+v", got)
	}
	if got.TopPages[0] != (Count{"/pricing", 25}) || got.Referrers[1] != (Count{"(none)", 7}) || len(got.Errors) != 0 {
		t.Fatalf("breakdowns=%+v", got)
	}
	if len(got.Retention) != 2 || got.Retention[0].Size != 5 || len(got.Retention[0].Active) != 2 || got.Retention[0].Active[1] != 3 || got.Retention[1].Active[0] != 2 {
		t.Fatalf("retention=%+v", got.Retention)
	}
	if got.Funnel[3] != (FunnelStep{"Machine paired", 3}) || len(got.Funnel) != 4 {
		t.Fatalf("funnel=%+v", got.Funnel)
	}
	if last := f.last.Load().(string); last != "Bearer phx_test_key|/api/projects/123/query/|HogQLQuery" {
		t.Fatalf("request=%q", last)
	}
	first := f.calls.Load()
	if first != 8 {
		t.Fatalf("one overview must be 8 queries, got %d", first)
	}
	if _, err = s.Overview(context.Background(), 30); err != nil || f.calls.Load() != first {
		t.Fatalf("second read within the TTL must hit the cache (calls %d, err %v)", f.calls.Load(), err)
	}
	if _, err = s.Overview(context.Background(), 7); err != nil || f.calls.Load() != first+8 {
		t.Fatalf("another range is its own cache entry (calls %d, err %v)", f.calls.Load(), err)
	}
	now = now.Add(cacheTTL + time.Second)
	if _, err = s.Overview(context.Background(), 30); err != nil || f.calls.Load() != first+16 {
		t.Fatalf("expired entry must refetch (calls %d, err %v)", f.calls.Load(), err)
	}
}

func TestOverviewKeepsPartialResultsAndNamesTheFailedInsight(t *testing.T) {
	now := time.Now()
	f := &fakePostHog{failFor: "geoip_country_code"}
	got, err := newTestService(f, &now).Overview(context.Background(), 7)
	if err != nil || got.Errors["countries"] != "provider_unavailable" || len(got.Errors) != 1 || got.Totals.PageViews != 40 || len(got.Countries) != 0 {
		t.Fatalf("partial=%+v err=%v", got, err)
	}
}

func TestOverviewFailsWhenEverythingFailsAndDoesNotCacheIt(t *testing.T) {
	now := time.Now()
	f := &fakePostHog{status: 401}
	s := newTestService(f, &now)
	if _, err := s.Overview(context.Background(), 7); !errors.Is(err, ErrProvider) || strings.Contains(err.Error(), "phx_test_key") {
		t.Fatalf("err=%v", err)
	}
	f.status = 0
	if got, err := s.Overview(context.Background(), 7); err != nil || got.Totals.PageViews != 40 {
		t.Fatalf("failure was cached: %+v %v", got, err)
	}
}

func TestOverviewRefusesUnlistedRangesAndMissingConfiguration(t *testing.T) {
	now := time.Now()
	s := newTestService(&fakePostHog{}, &now)
	for _, days := range []int{-1, 1, 8, 365} {
		if _, err := s.Overview(context.Background(), days); !errors.Is(err, ErrInvalid) {
			t.Errorf("days=%d err=%v", days, err)
		}
	}
	var unset *Service
	if _, err := unset.Overview(context.Background(), 7); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("nil service err=%v", err)
	}
	if st := New(Config{Host: "https://posthog.example.test"}).Status(); st.Configured {
		t.Fatalf("host without project and key must not count as configured: %+v", st)
	}
	if st := s.Status(); !st.Configured || st.Host == "" {
		t.Fatalf("status=%+v", st)
	}
}

func TestQueryNeverFollowsARedirectWithTheKey(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(r.Header.Get("Authorization") != "")
		w.Write([]byte(`{"results":[]}`))
	}))
	defer other.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) }))
	defer redirect.Close()
	s := New(Config{Host: redirect.URL, ProjectID: "1", Key: "k", Client: redirect.Client()})
	if _, err := s.Overview(context.Background(), 7); !errors.Is(err, ErrProvider) || leaked.Load() {
		t.Fatalf("redirect followed: leaked=%v err=%v", leaked.Load(), err)
	}
}
