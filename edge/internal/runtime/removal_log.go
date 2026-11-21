package runtime

import "log/slog"

// removalLogDetail is how many removals of one event are logged with their ids; the rest only count.
const removalLogDetail = 10

// removalLog logs one event's route removals: the first removalLogDetail with ids, then (past that)
// one summary line, so an edge restart that drops thousands of routes logs a bounded number of lines.
type removalLog struct {
	msg     string
	n       int
	reasons map[string]int
}

func (l *removalLog) add(route, endpoint, reason string) {
	l.n++
	if l.reasons == nil {
		l.reasons = map[string]int{}
	}
	l.reasons[reason]++
	if l.n <= removalLogDetail {
		slog.Info(l.msg, "route_id", route, "endpoint_id", endpoint, "reason", reason)
	}
}

func (l *removalLog) flush() {
	if l.n > removalLogDetail {
		slog.Info(l.msg, "removed_count", l.n, "reasons", l.reasons)
	}
}
