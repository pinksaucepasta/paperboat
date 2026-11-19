package transactionalmail

import (
	"testing"
	"time"
)

func TestTimeAndDayReadForPeople(t *testing.T) {
	zone := time.FixedZone("IST", 5*3600+1800)
	at := time.Date(2026, 12, 19, 15, 30, 0, 0, zone) // 10:00 UTC
	if got := Time(at); got != "19 December 2026, 10:00 UTC" {
		t.Fatalf("Time = %q", got)
	}
	if got := Day(time.Date(2027, 1, 5, 0, 0, 0, 0, time.UTC)); got != "5 January 2027" {
		t.Fatalf("Day = %q", got)
	}
}
