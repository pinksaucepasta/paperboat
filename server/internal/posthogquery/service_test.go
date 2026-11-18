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
	case strings.Contains(q, "INTERVAL 5 MINUTE"):
		w.Write([]byte(`{"results":[[3]]}`))
	case strings.Contains(q, "uniqIf(distinct_id, event='$pageview'"):
		w.Write([]byte(`{"results":[[120,100]]}`))
	case strings.Contains(q, "event='signed_in' AND properties.app='server' GROUP BY distinct_id HAVING first"):
		w.Write([]byte(`{"results":[[12,8]]}`))
	case strings.Contains(q, "toDate(timestamp)"):
		w.Write([]byte(`{"results":[["2026-11-16",10],["2026-11-17",30]]}`))
	case strings.Contains(q, "signup_clicked"):
		w.Write([]byte(`{"results":[["news.example.test",40,5],["$direct",30,2]]}`))
	case strings.Contains(q, "GROUP BY name HAVING"):
		w.Write([]byte(`{"results":[["/pricing",25],["/",7]]}`))
	case strings.Contains(q, "INTERVAL 1 DAY"):
		w.Write([]byte(`{"results":[[4,3,9,6]]}`))
	case strings.Contains(q, "checkout_completed"):
		w.Write([]byte(`{"results":[[2,1]]}`))
	case strings.Contains(q, "countIf(machines > 0)"):
		w.Write([]byte(`{"results":[[10,6,2]]}`))
	case strings.Contains(q, "GROUP BY recent"):
		w.Write([]byte(`{"results":[[true,8,5],[false,6,2]]}`))
	case strings.Contains(q, "event='feature_used'"):
		w.Write([]byte(`{"results":[["terminal",7],["tunnel","2"]]}`))
	default:
		w.Write([]byte(`{"results":[[20]]}`))
	}
}

func newTestService(f *fakePostHog, now *time.Time) *Service {
	server := httptest.NewTLSServer(f)
	return New(Config{Host: server.URL, ProjectID: "123", Key: "phx_test_key", Client: server.Client(), Now: func() time.Time { return *now }})
}

func TestSiteParsesInsightsSendsKeyAndCaches(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	f := &fakePostHog{}
	s := newTestService(f, &now)
	got, err := s.Site(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Days != 30 || got.Visitors != (Pair{120, 100}) || got.Signups != (Pair{12, 8}) || got.Online != 3 || len(got.Daily) != 2 || got.Daily[1].Visitors != 30 || len(got.Errors) != 0 {
		t.Fatalf("site=%+v", got)
	}
	if got.Referrers[0] != (Source{"news.example.test", 40, 5}) || got.Pages[0] != (Count{"/pricing", 25}) || got.Countries[1] != (Count{"/", 7}) {
		t.Fatalf("breakdowns=%+v", got)
	}
	if last := f.last.Load().(string); last != "Bearer phx_test_key|/api/projects/123/query/|HogQLQuery" {
		t.Fatalf("request=%q", last)
	}
	first := f.calls.Load()
	if first != 8 {
		t.Fatalf("one site read must be 7 insights and 1 online query, got %d", first)
	}
	if _, err = s.Site(context.Background(), 30); err != nil || f.calls.Load() != first {
		t.Fatalf("second read within the TTL must hit the cache (calls %d, err %v)", f.calls.Load(), err)
	}
	if _, err = s.Site(context.Background(), 7); err != nil || f.calls.Load() != first+7 {
		t.Fatalf("another range is its own cache entry, the online count is shared (calls %d, err %v)", f.calls.Load(), err)
	}
	now = now.Add(cacheTTL + time.Second)
	if _, err = s.Site(context.Background(), 30); err != nil || f.calls.Load() != first+7+8 {
		t.Fatalf("expired entry must refetch (calls %d, err %v)", f.calls.Load(), err)
	}
}

func TestConsoleParsesFunnelRetentionAndAdoption(t *testing.T) {
	now := time.Now()
	got, err := newTestService(&fakePostHog{}, &now).Console(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if got.DailyActive != (Pair{4, 3}) || got.WeeklyActive != (Pair{9, 6}) || got.Paid != (Pair{2, 1}) || got.ActiveAccounts != 20 || len(got.Errors) != 0 {
		t.Fatalf("console=%+v", got)
	}
	if len(got.Funnel) != 3 || got.Funnel[1] != (FunnelStep{"Added a machine", 6}) || got.Funnel[2].Count != 2 {
		t.Fatalf("funnel=%+v", got.Funnel)
	}
	if got.Retention != (Retention{8, 5, 6, 2}) || got.Adoption[1] != (Count{"tunnel", 2}) {
		t.Fatalf("retention=%+v adoption=%+v", got.Retention, got.Adoption)
	}
}

func TestSiteKeepsPartialResultsAndNamesTheFailedInsight(t *testing.T) {
	now := time.Now()
	f := &fakePostHog{failFor: "geoip_country_code"}
	got, err := newTestService(f, &now).Site(context.Background(), 7)
	if err != nil || got.Errors["countries"] != "provider_unavailable" || len(got.Errors) != 1 || got.Visitors.Value != 120 || len(got.Countries) != 0 {
		t.Fatalf("partial=%+v err=%v", got, err)
	}
}

func TestSiteFailsWhenEverythingFailsAndDoesNotCacheIt(t *testing.T) {
	now := time.Now()
	f := &fakePostHog{status: 401}
	s := newTestService(f, &now)
	if _, err := s.Site(context.Background(), 7); !errors.Is(err, ErrProvider) || strings.Contains(err.Error(), "phx_test_key") {
		t.Fatalf("err=%v", err)
	}
	f.status = 0
	if got, err := s.Site(context.Background(), 7); err != nil || got.Visitors.Value != 120 {
		t.Fatalf("failure was cached: %+v %v", got, err)
	}
}

func TestRefusesUnlistedRangesAndMissingConfiguration(t *testing.T) {
	now := time.Now()
	s := newTestService(&fakePostHog{}, &now)
	for _, days := range []int{-1, 1, 8, 365} {
		if _, err := s.Site(context.Background(), days); !errors.Is(err, ErrInvalid) {
			t.Errorf("site days=%d err=%v", days, err)
		}
		if _, err := s.Console(context.Background(), days); !errors.Is(err, ErrInvalid) {
			t.Errorf("console days=%d err=%v", days, err)
		}
	}
	var unset *Service
	if _, err := unset.Console(context.Background(), 7); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("nil service err=%v", err)
	}
	if st := New(Config{Host: "https://posthog.example.test"}).Status(); st.Configured {
		t.Fatalf("host without project and key must not count as configured: %+v", st)
	}
	if st := s.Status(); !st.Configured || st.Host == "" || !strings.HasSuffix(st.URL, "/project/123") {
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
	if _, err := s.Site(context.Background(), 7); !errors.Is(err, ErrProvider) || leaked.Load() {
		t.Fatalf("redirect followed: leaked=%v err=%v", leaked.Load(), err)
	}
}

func TestDailyActiveCompareIsDayOverDay(t *testing.T) {
	q := activeQuery()
	if !strings.Contains(q, "INTERVAL 2 DAY AND timestamp < now() - INTERVAL 1 DAY") || !strings.Contains(q, "INTERVAL 14 DAY AND timestamp < now() - INTERVAL 7 DAY") {
		t.Fatalf("each previous period must be bounded on both sides: %s", q)
	}
}
