package admission

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

// F21-02: admission deadlines are server-issued expiries. With this machine's clock 10 min ahead of the server,
// a credential with a minute left was refused as expired; 10 min behind, an expired one was admitted.
func TestAdmissionJudgesDeadlinesOnTheServerClock(t *testing.T) {
	t.Cleanup(func() { serverclock.Observe(time.Now().UTC().Format(http.TimeFormat), time.Now(), time.Now()) })
	for name, clientAhead := range map[string]time.Duration{"client 10 min ahead": 10 * time.Minute, "client 10 min behind": -10 * time.Minute} {
		t.Run(name, func(t *testing.T) {
			serverclock.Observe(time.Now().Add(-clientAhead).UTC().Format(http.TimeFormat), time.Now(), time.Now())
			fresh, stale := serverclock.Now().Add(time.Minute), serverclock.Now().Add(-time.Minute)
			lease, err := Admit(context.Background(), fresh, nil)
			if err != nil {
				t.Fatalf("fresh server credential refused: %v", err)
			}
			lease.Close()
			if _, err := Admit(context.Background(), stale, nil); !errors.Is(err, ErrExpired) {
				t.Fatalf("expired server credential admitted: %v", err)
			}
			staged, err := Stage(context.Background(), fresh)
			if err != nil {
				t.Fatalf("staging refused a fresh server credential: %v", err)
			}
			if _, err := staged.Accept(); err != nil {
				t.Fatalf("accept refused a fresh server credential: %v", err)
			}
			staged.Abort()
		})
	}
}
