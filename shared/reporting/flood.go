package reporting

import (
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Flood limits. Sentry's free plan holds 5,000 errors a month (about seven an hour for the whole fleet); on
// 2026-11-05 one crash-looping daemon (systemd RestartSec=5s, a new process every 5 s, so no per-process counter
// ever filled) sent 4,135 identical events and blocked every error for the rest of the month.
const (
	floodBurst  = 3        // the same fault is sent this many times at once, then once an hour,
	floodBudget = 30       // with a count of what was held back; errors of every kind per hour
	floodKeys   = 64       // distinct faults remembered; the least recently used is forgotten first
	floodMaxLen = 64 << 10 // a state file larger than this is ignored
)

// FloodGuard decides whether an error event may be sent. It is a token bucket per fault key plus one bucket for all
// of them, the shape of Tailscale's types/logger RateLimitedFn (a bucket per format string, a bounded LRU of
// buckets, the number of suppressed messages reported on the next one that passes). With a state file the buckets
// survive a restart, which is what stops a crash-loop; without one (or when the file cannot be written, for example
// on a read-only file system) they live for the process.
type FloodGuard struct {
	mu    sync.Mutex
	path  string
	now   func() time.Time
	state floodState
}

type floodBucket struct {
	Tokens     float64 `json:"tokens"`
	At         int64   `json:"at"` // unix seconds of the last refill
	Suppressed uint32  `json:"suppressed,omitempty"`
}

type floodState struct {
	Budget floodBucket             `json:"budget"`
	Keys   map[string]*floodBucket `json:"keys"`
}

// NewFloodGuard returns a guard; path "" keeps the state in memory only.
func NewFloodGuard(path string) *FloodGuard {
	return &FloodGuard{path: path, now: time.Now}
}

// SetPath makes the guard persist its state in path ("" stops persisting).
func (g *FloodGuard) SetPath(path string) {
	g.mu.Lock()
	g.path = path
	g.mu.Unlock()
}

// Admit reports whether the event for key may be sent and, when it may, how many events for that key were held
// back since the last one that was sent.
func (g *FloodGuard) Admit(key string) (ok bool, suppressed uint32) {
	if g == nil {
		return true, 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now().Unix()
	g.load()
	if g.state.Keys == nil {
		g.state.Keys = map[string]*floodBucket{}
	}
	bucket := g.state.Keys[key]
	if bucket == nil {
		bucket = &floodBucket{Tokens: floodBurst, At: now}
		g.evict()
		g.state.Keys[key] = bucket
	}
	refill(bucket, now, floodBurst, 1)
	if g.state.Budget.At == 0 {
		g.state.Budget = floodBucket{Tokens: floodBudget, At: now}
	}
	refill(&g.state.Budget, now, floodBudget, floodBudget)
	defer g.save()
	if bucket.Tokens < 1 || g.state.Budget.Tokens < 1 {
		bucket.Suppressed = min(bucket.Suppressed, math.MaxUint32-1) + 1
		return false, 0
	}
	bucket.Tokens--
	g.state.Budget.Tokens--
	suppressed, bucket.Suppressed = bucket.Suppressed, 0
	return true, suppressed
}

// refill adds perHour tokens an hour, up to capacity. A clock that moved backwards adds nothing.
func refill(b *floodBucket, now int64, capacity, perHour float64) {
	if elapsed := now - b.At; elapsed > 0 {
		b.Tokens = min(capacity, b.Tokens+float64(elapsed)*perHour/3600)
	}
	b.At = max(b.At, now)
}

func (g *FloodGuard) evict() {
	for len(g.state.Keys) >= floodKeys {
		oldestKey, oldest := "", int64(math.MaxInt64)
		for key, bucket := range g.state.Keys {
			if bucket.At < oldest {
				oldestKey, oldest = key, bucket.At
			}
		}
		delete(g.state.Keys, oldestKey)
	}
}

// load replaces the state with the file's, so concurrent processes of one machine share it. A missing, oversized
// or unreadable file leaves the in-memory state in place.
func (g *FloodGuard) load() {
	if g.path == "" {
		return
	}
	file, err := os.Open(g.path)
	if err != nil {
		return
	}
	defer file.Close()
	var state floodState
	if json.NewDecoder(io.LimitReader(file, floodMaxLen)).Decode(&state) != nil || len(state.Keys) > floodKeys {
		return
	}
	for _, bucket := range state.Keys {
		if bucket == nil || math.IsNaN(bucket.Tokens) || math.IsNaN(state.Budget.Tokens) {
			return
		}
	}
	g.state = state
}

// save writes the state best effort: a flood guard must never be the reason a failure is lost or a process stops.
func (g *FloodGuard) save() {
	if g.path == "" {
		return
	}
	data, err := json.Marshal(g.state)
	if err != nil {
		return
	}
	temp, err := os.CreateTemp(filepath.Dir(g.path), ".sentry-flood-*")
	if err != nil {
		return
	}
	_, werr := temp.Write(data)
	if cerr := temp.Close(); werr != nil || cerr != nil || os.Rename(temp.Name(), g.path) != nil {
		_ = os.Remove(temp.Name())
	}
}
