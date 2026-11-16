package usage

import "time"

// MaxDeadLetters bounds the refused reports an edge keeps for audit; the oldest go first, the count of refusals does not.
const MaxDeadLetters = 256

// DeadLetter is a report the control plane refused for good (a malformed report or a signature it cannot verify).
// Retrying it can never succeed, so it leaves the queue and is kept here instead of blocking the reports behind it.
type DeadLetter struct {
	Report Report    `json:"report"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// Refuse moves a pending report to the dead letters; it reports false when the report is not pending.
func (q *Queue) Refuse(operationID, reason string, at time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	report, ok := q.remove(operationID)
	if !ok {
		return false
	}
	if len(q.dead) >= MaxDeadLetters {
		q.dead = append(q.dead[:0], q.dead[1:]...)
	}
	q.dead = append(q.dead, DeadLetter{Report: report, Reason: reason, At: at})
	q.refused++
	return true
}

// DeadLetters returns a copy of the kept refused reports, oldest first.
func (q *Queue) DeadLetters() []DeadLetter {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]DeadLetter(nil), q.dead...)
}
