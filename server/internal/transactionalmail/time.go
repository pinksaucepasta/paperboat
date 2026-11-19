package transactionalmail

import "time"

// Time writes an instant for a person: "19 December 2026, 10:00 UTC". Every moment in a mail goes through it.
func Time(t time.Time) string { return t.UTC().Format("2 January 2006, 15:04") + " UTC" }

// Day writes a calendar day with no clock: "19 December 2026".
func Day(t time.Time) string { return t.Format("2 January 2006") }
