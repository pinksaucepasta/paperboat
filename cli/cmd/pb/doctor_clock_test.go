package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

func shortClockTimeouts(t *testing.T) {
	t.Helper()
	old := doctorClockTimeout
	doctorClockTimeout = 200 * time.Millisecond
	t.Cleanup(func() { doctorClockTimeout = old })
}

func TestMeasureServerClockUsesThisResponsesDate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Date", time.Now().Add(-2*time.Hour).UTC().Format(http.TimeFormat))
	}))
	defer server.Close()
	offset, err := measureServerClock(context.Background(), server.URL)
	if err != nil || offset > -119*time.Minute || offset < -121*time.Minute {
		t.Fatalf("offset %v, err %v: want about -2h from this response", offset, err)
	}
}

func TestMeasureServerClockSaysWhyItFailed(t *testing.T) {
	shortClockTimeouts(t)
	for name, tc := range map[string]struct {
		date    []string // nil: the server sends its own Date
		missing bool
		want    error
		summary string
	}{
		"no date":  {missing: true, want: serverclock.ErrNoDate, summary: "carries no time"},
		"bad date": {date: []string{"yesterday-ish"}, want: serverclock.ErrBadDate, summary: "could not be read, so its clock"},
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header()["Date"] = tc.date // nil suppresses the automatic Date
			}))
			defer server.Close()
			_, err := measureServerClock(context.Background(), server.URL)
			if !errors.Is(err, tc.want) || calls.Load() != 1 {
				t.Fatalf("err %v after %d calls, want %v after 1", err, calls.Load(), tc.want)
			}
			if got := clockUnreadableSummary(err); !strings.Contains(got, tc.summary) {
				t.Fatalf("summary %q lacks %q", got, tc.summary)
			}
		})
	}
}

func TestMeasureServerClockRetriesATimeoutOnce(t *testing.T) {
	shortClockTimeouts(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			<-r.Context().Done()
			return
		}
	}))
	defer server.Close()
	if _, err := measureServerClock(context.Background(), server.URL); err != nil || calls.Load() != 2 {
		t.Fatalf("err %v after %d calls, want success on the second", err, calls.Load())
	}
}

func TestMeasureServerClockTimesOutTwiceWithItsOwnWording(t *testing.T) {
	shortClockTimeouts(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-r.Context().Done()
	}))
	defer server.Close()
	_, err := measureServerClock(context.Background(), server.URL)
	if err == nil || calls.Load() != 2 || !isTimeout(err) {
		t.Fatalf("err %v after %d calls, want a timeout after exactly 2", err, calls.Load())
	}
	if got := clockUnreadableSummary(err); !strings.Contains(got, "did not answer in time") {
		t.Fatalf("summary %q", got)
	}
}

func TestDoctorNamesTheClockOffset(t *testing.T) {
	for offset, want := range map[time.Duration]string{
		10 * time.Minute: "10 minutes behind",
		-time.Minute:     "1 minute ahead",
		-3 * time.Hour:   "3 hours ahead",
		90 * time.Second: "2 minutes behind",
	} {
		if got := describeClockOffset(offset); got != want {
			t.Errorf("%v: %q, want %q", offset, got, want)
		}
	}
}

func TestClockUnreadableSummaryNamesASlowConnection(t *testing.T) {
	got := clockUnreadableSummary(fmt.Errorf("probe: %w", serverclock.ErrSlowRoundTrip))
	if want := "The connection to the Paperboat server was too slow to compare clocks accurately."; got != want {
		t.Fatalf("%q, want %q", got, want)
	}
}
