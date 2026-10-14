package operatorreporting

import (
	"errors"
	"testing"
	"time"
)

func TestProjectOutcomesProjectsFixedVocabularyAndAggregatesUnknownLabels(t *testing.T) {
	t0 := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	raw := sentryOutcomeResponse{Start: t0, End: t0.Add(2 * time.Hour), Intervals: []time.Time{t0, t0.Add(time.Hour)}, Groups: []sentryOutcomeGroup{
		{By: map[string]string{"category": "error", "outcome": "rate_limited", "reason": "quota"}, Series: map[string][]float64{"sum(quantity)": {2, 3}}},
		{By: map[string]string{"category": "secret-category", "outcome": "rate_limited", "reason": "customer@example.test"}, Series: map[string][]float64{"sum(quantity)": {4, 5}}},
		{By: map[string]string{"category": "another-secret", "outcome": "rate_limited", "reason": "another-secret"}, Series: map[string][]float64{"sum(quantity)": {6, 7}}},
	}}
	got, err := projectOutcomes(raw, 2, time.Hour)
	if err != nil || len(got) != 4 {
		t.Fatalf("got %#v err=%v", got, err)
	}
	if got[0].Category != "error" || got[0].Reason != "quota" || got[0].Quantity != 2 || got[1].Category != "other" || got[1].Reason != "other" || got[1].Quantity != 10 || got[3].Quantity != 12 {
		t.Fatalf("projection = %#v", got)
	}
}

func TestProjectOutcomesRejectsUnboundedOrMalformedProviderData(t *testing.T) {
	t0 := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	valid := sentryOutcomeGroup{By: map[string]string{"category": "error", "outcome": "accepted", "reason": ""}, Series: map[string][]float64{"sum(quantity)": {1}}}
	base := func(intervals []time.Time, groups []sentryOutcomeGroup) sentryOutcomeResponse {
		return sentryOutcomeResponse{Start: t0, End: t0.Add(time.Hour), Intervals: intervals, Groups: groups}
	}
	tests := map[string]sentryOutcomeResponse{
		"missing intervals":   {},
		"unknown outcome":     base([]time.Time{t0}, []sentryOutcomeGroup{{By: map[string]string{"category": "error", "outcome": "secret"}, Series: map[string][]float64{"sum(quantity)": {1}}}}),
		"series mismatch":     base([]time.Time{t0}, []sentryOutcomeGroup{{By: valid.By, Series: map[string][]float64{"sum(quantity)": {1, 2}}}}),
		"negative quantity":   base([]time.Time{t0}, []sentryOutcomeGroup{{By: valid.By, Series: map[string][]float64{"sum(quantity)": {-1}}}}),
		"unordered intervals": base([]time.Time{t0, t0}, []sentryOutcomeGroup{{By: valid.By, Series: map[string][]float64{"sum(quantity)": {1, 1}}}}),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := projectOutcomes(raw, 1, time.Hour); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	if _, err := projectOutcomes(base([]time.Time{t0}, make([]sentryOutcomeGroup, maxOutcomeGroups+1)), 1, time.Hour); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("too many groups err = %v", err)
	}
	if _, err := projectOutcomes(base(make([]time.Time, 3), nil), 1, time.Hour); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("too many points err = %v", err)
	}
}

func TestProjectOutcomesAcceptsLiveInclusiveBoundaryBuckets(t *testing.T) {
	start := time.Date(2026, 9, 18, 16, 23, 0, 0, time.UTC)
	intervals, values := make([]time.Time, 25), make([]float64, 25)
	first := time.Date(2026, 9, 18, 16, 0, 0, 0, time.UTC)
	for i := range intervals {
		intervals[i], values[i] = first.Add(time.Duration(i)*time.Hour), float64(i)
	}
	raw := sentryOutcomeResponse{Start: start, End: start.Add(24 * time.Hour), Intervals: intervals, Groups: []sentryOutcomeGroup{{By: map[string]string{"category": "transaction_indexed", "outcome": "invalid", "reason": "invalid_transaction"}, Series: map[string][]float64{"sum(quantity)": values}}}}
	got, err := projectOutcomes(raw, 24, time.Hour)
	if err != nil || len(got) != 25 || got[0].Category != "transaction_indexed" || got[0].Reason != "invalid_transaction" {
		t.Fatalf("live projection=%#v err=%v", got, err)
	}
}
