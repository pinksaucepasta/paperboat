package operatorreporting

import (
	"context"
	"math"
	"net/url"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
)

const maxOutcomeGroups = 100

// IngestionOutcome is a provider-reported ingestion count. It is operational
// evidence, not subscription usage or billing data.
type IngestionOutcome struct {
	Timestamp time.Time `json:"timestamp"`
	Category  string    `json:"category"`
	Outcome   string    `json:"outcome"`
	Reason    string    `json:"reason"`
	Quantity  float64   `json:"quantity"`
}

type IngestionOutcomeSeries struct {
	Project         string             `json:"project"`
	Period          string             `json:"period"`
	IntervalSeconds int64              `json:"interval_seconds"`
	Outcomes        []IngestionOutcome `json:"outcomes"`
}

type sentryOutcomeResponse struct {
	Start     time.Time            `json:"start"`
	End       time.Time            `json:"end"`
	Intervals []time.Time          `json:"intervals"`
	Groups    []sentryOutcomeGroup `json:"groups"`
}

type sentryOutcomeGroup struct {
	By     map[string]string    `json:"by"`
	Series map[string][]float64 `json:"series"`
}

var outcomePeriods = map[string]struct {
	statsPeriod string
	interval    string
	seconds     int64
	points      int
}{
	"24h": {statsPeriod: "24h", interval: "1h", seconds: 3600, points: 24},
	"7d":  {statsPeriod: "7d", interval: "1d", seconds: 86400, points: 7},
}

var allowedOutcomes = map[string]bool{
	"accepted": true, "filtered": true, "rate_limited": true, "invalid": true,
	"abuse": true, "client_discard": true, "cardinality_limited": true,
}

var allowedOutcomeCategories = map[string]bool{
	"error": true, "transaction": true, "attachment": true, "replay": true,
	"profile": true, "profile_duration": true, "profile_duration_ui": true,
	"profile_chunk": true, "profile_chunk_ui": true, "monitor": true,
	"span": true, "span_indexed": true, "transaction_indexed": true,
	"log_item": true, "log_byte": true, "metric_bucket": true,
	"trace_metric": true, "trace_metric_byte": true,
}

// Reasons are intentionally classified instead of passing provider-controlled
// labels through the operator API.
var allowedOutcomeReasons = map[string]bool{
	"none": true, "sample_rate": true, "before_send": true, "event_processor": true,
	"network_error": true, "queue_overflow": true, "cache_overflow": true,
	"ratelimit_backoff": true, "insufficient_data": true, "quota": true,
	"project_id": true, "invalid_event": true, "invalid_transaction": true,
	"discarded": true,
}

func (s *Service) IngestionOutcomes(ctx context.Context, actor auth.User, projectSlug, period string) (IngestionOutcomeSeries, error) {
	out := IngestionOutcomeSeries{Project: projectSlug, Period: period, Outcomes: []IngestionOutcome{}}
	window, ok := outcomePeriods[period]
	if !ok {
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
	q := url.Values{
		"project":     {project.ID},
		"field":       {"sum(quantity)"},
		"groupBy":     {"category", "outcome", "reason"},
		"statsPeriod": {window.statsPeriod},
		"interval":    {window.interval},
	}
	var raw sentryOutcomeResponse
	if _, err := s.get(ctx, "/api/0/organizations/"+url.PathEscape(s.org)+"/stats_v2/", q, &raw); err != nil {
		if reportingErrorOnly(err, errProviderRejected, ErrNotFound) {
			return out, ErrUnsupported
		}
		return out, err
	}
	out.IntervalSeconds = window.seconds
	out.Outcomes, err = projectOutcomes(raw, window.points, time.Duration(window.seconds)*time.Second)
	if err != nil {
		return IngestionOutcomeSeries{}, err
	}
	if err := s.auditRemote(ctx, actor, "sentry_ingestion_outcomes", project.Slug+":"+period, len(out.Outcomes)); err != nil {
		return IngestionOutcomeSeries{}, err
	}
	return out, nil
}

func projectOutcomes(raw sentryOutcomeResponse, periodPoints int, step time.Duration) ([]IngestionOutcome, error) {
	if len(raw.Intervals) == 0 || len(raw.Intervals) > periodPoints+1 || len(raw.Groups) > maxOutcomeGroups || !validProviderTime(raw.Start) || !validProviderTime(raw.End) || !raw.End.After(raw.Start) {
		return nil, ErrUnavailable
	}
	requestedDuration := time.Duration(periodPoints) * step
	if duration := raw.End.Sub(raw.Start); duration < requestedDuration-step || duration > requestedDuration+step {
		return nil, ErrUnavailable
	}
	for i, timestamp := range raw.Intervals {
		if !validProviderTime(timestamp) || timestamp.Before(raw.Start.Add(-step)) || timestamp.After(raw.End.Add(step)) || i > 0 && timestamp.Sub(raw.Intervals[i-1]) != step {
			return nil, ErrUnavailable
		}
	}
	// Provider reasons outside the explicit vocabulary are merged into "other";
	// category labels are treated the same way. This preserves the count without
	// exposing provider-controlled labels.
	type key struct {
		point                     int
		category, outcome, reason string
	}
	counts := make(map[key]float64)
	for _, group := range raw.Groups {
		outcome := group.By["outcome"]
		if !allowedOutcomes[outcome] {
			return nil, ErrUnavailable
		}
		category := group.By["category"]
		if !allowedOutcomeCategories[category] {
			category = "other"
		}
		reason := group.By["reason"]
		if reason == "" {
			reason = "none"
		} else if !allowedOutcomeReasons[reason] {
			reason = "other"
		}
		series, ok := group.Series["sum(quantity)"]
		if !ok || len(series) != len(raw.Intervals) {
			return nil, ErrUnavailable
		}
		for i, quantity := range series {
			if math.IsNaN(quantity) || math.IsInf(quantity, 0) || quantity < 0 || quantity > 9e15 {
				return nil, ErrUnavailable
			}
			counts[key{i, category, outcome, reason}] += quantity
		}
	}
	out := make([]IngestionOutcome, 0, len(counts))
	for i, timestamp := range raw.Intervals {
		for _, category := range []string{"error", "transaction", "transaction_indexed", "attachment", "replay", "profile", "profile_duration", "profile_duration_ui", "profile_chunk", "profile_chunk_ui", "monitor", "span", "span_indexed", "log_item", "log_byte", "metric_bucket", "trace_metric", "trace_metric_byte", "other"} {
			for _, outcome := range []string{"accepted", "filtered", "rate_limited", "invalid", "abuse", "client_discard", "cardinality_limited"} {
				for _, reason := range []string{"none", "sample_rate", "before_send", "event_processor", "network_error", "queue_overflow", "cache_overflow", "ratelimit_backoff", "insufficient_data", "quota", "project_id", "invalid_event", "invalid_transaction", "discarded", "other"} {
					if quantity, ok := counts[key{i, category, outcome, reason}]; ok {
						out = append(out, IngestionOutcome{Timestamp: timestamp, Category: category, Outcome: outcome, Reason: reason, Quantity: quantity})
					}
				}
			}
		}
	}
	return out, nil
}

func validProviderTime(v time.Time) bool {
	return !v.IsZero() && v.Year() >= 2000 && v.Year() <= 2100
}
