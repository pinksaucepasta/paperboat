package app

import (
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/workers"
)

// Every leader loop must be countable (F21-55): a worker name missing from the progress list is silently
// unregistered, so BeginIteration never reports its runs.
func TestEveryLeaderWorkerIsRegisteredForIterationCounts(t *testing.T) {
	source, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, m := range regexp.MustCompile(`observedWorker\(supportService, "(worker_[a-z_]+)"`).FindAllStringSubmatch(string(source), -1) {
		names[m[1]] = true
	}
	if len(names) < 20 {
		t.Fatalf("found only %d workers in app.go", len(names))
	}
	progress := workers.NewProgress(time.Minute)
	for name := range names {
		progress.Register(name)
	}
	if got := len(progress.Snapshot()); got != len(names) {
		t.Fatalf("%d of %d workers registered for progress", got, len(names))
	}
}
