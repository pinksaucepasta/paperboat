package supportreport

import "github.com/getsentry/sentry-go"

// NewFaultEvent builds an exception exclusively from an already projected
// fault. Validation and source-stack redaction share the service's event
// policy; the underlying error and its message are never evaluated here.
func NewFaultEvent(fault Fault, stack *sentry.Stacktrace) *sentry.Event {
	return sanitize(&sentry.Event{
		Level:       sentry.LevelError,
		Tags:        faultTags(fault),
		Contexts:    map[string]sentry.Context{"fault": faultContext(fault)},
		Exception:   []sentry.Exception{{Type: fault.ErrorType, Value: fault.Cause, Stacktrace: stack}},
		Fingerprint: []string{"{{ default }}", fault.Component, fault.Operation, fault.Stage, fault.Code, fault.Cause},
	})
}

// SanitizeFaultEvent admits only the bounded fault schema and reconstructs
// every exported field, including stack frames. Ordinary SDK events without
// authoritative fault metadata are rejected.
func SanitizeFaultEvent(event *sentry.Event) *sentry.Event {
	if event == nil {
		return nil
	}
	return sanitize(event)
}
