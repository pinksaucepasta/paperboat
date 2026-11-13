package reporting

import (
	"strings"

	"github.com/getsentry/sentry-go"
)

// Extension is what one service adds to the shared model: its failure
// definitions, bounded vocabularies, typed-error classes and metrics. Services
// register once at startup (init); the shared package owns everything else.
type Extension struct {
	Definitions map[string]Definition
	Operations  []string
	Codes       []string
	Causes      []string
	// Stages maps a stage to its failure name; %s is replaced by the service name.
	Stages map[string]string
	// Classify names the cause of the service's own typed errors; rank 0 means unknown.
	Classify func(error) (string, int)
	// Reference returns a support reference carried by one of the service's own errors.
	Reference func(error) string
	// Lifecycles validates a bounded lifecycle operation: code and outcome must agree.
	Lifecycles map[string]func(code, outcome string) (retry, severity string, ok bool)
	Metrics    []Metric
}

// Metric is one allowed Sentry metric and its allowed label values.
type Metric struct {
	Name   string
	Kind   sentry.MetricType
	Labels map[string][]string
}

var (
	components = map[string]bool{"paperboat-relay": true, "paperboat-edge": true, "paperboat-selfhost": true}
	outcomes   = values("success", "failed", "rejected", "canceled", "state_change")
	causes     = values("internal", "process_panic", "context_canceled", "deadline_exceeded", "server_closed", "not_found", "permission_denied", "conflict", "rate_limited", "service_unavailable", "invalid_request", "upstream_failure", "connection_closed", "end_of_stream", "truncated_stream", "connection_refused", "connection_reset", "broken_pipe", "network_unreachable", "address_in_use", "invalid_argument", "resource_exhausted", "network_timeout", "dns_failure", "system_error")
	stageNames = map[string]string{
		"control_request": "%s_control_request_failed", "serve": "%s_service_failed", "configure": "%s_service_setup_failed",
		"startup": "%s_service_start_failed", "process": "%s_process_panicked", "unknown": "%s_failure",
	}
	classifiers []func(error) (string, int)
	referencers []func(error) string
	lifecycles  = map[string]func(code, outcome string) (string, string, bool){}
)

// Register adds a service's extension. Call it from init, before any Reporter exists.
func Register(e Extension) {
	for key, definition := range e.Definitions {
		failureDefinitions[key] = definition
	}
	for _, v := range e.Operations {
		operations[v] = true
	}
	for _, v := range e.Codes {
		codes[v] = true
	}
	for _, v := range e.Causes {
		causes[v] = true
	}
	for stage, name := range e.Stages {
		stageNames[stage] = name
	}
	if e.Classify != nil {
		classifiers = append(classifiers, e.Classify)
	}
	if e.Reference != nil {
		referencers = append(referencers, e.Reference)
	}
	for operation, check := range e.Lifecycles {
		operations[operation] = true
		lifecycles[operation] = check
	}
	for _, m := range e.Metrics {
		labels := map[string]map[string]bool(nil)
		if m.Labels != nil {
			labels = map[string]map[string]bool{}
			for name, allowed := range m.Labels {
				labels[name] = values(allowed...)
			}
		}
		metricCatalog[m.Name] = metricSpec{m.Kind, labels}
	}
}

// SameError reports whether err is exactly target (comparable types only), never by message.
func SameError(err, target error) bool { return sameError(err, target) }

func domainCause(err error) (string, int) {
	for _, classify := range classifiers {
		if cause, rank := classify(err); rank > 0 {
			return cause, rank
		}
	}
	return "", 0
}

func requestReference(err error) string {
	for _, reference := range referencers {
		if value := reference(err); validReference(value) {
			return value
		}
	}
	return ""
}

func serviceName(component, name string) string {
	if !strings.Contains(name, "%s") {
		return name
	}
	service := "relay"
	if component == "paperboat-edge" {
		service = "edge"
	}
	return strings.Replace(name, "%s", service, 1)
}
