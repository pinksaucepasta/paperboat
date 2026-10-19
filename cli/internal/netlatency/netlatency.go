// Package netlatency measures how long this machine takes to reach the relays it can use and
// reports the times to the control plane for display. The numbers never choose a path.
package netlatency

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"sort"
	"sync"
	"time"
)

const (
	// FirstReportDelay lets the daemon finish starting before the first probe.
	FirstReportDelay = 30 * time.Second
	// Interval is the usual time between reports; each wait varies by up to 20% so machines spread out.
	Interval = 5 * time.Minute
	// ProbeTimeout bounds one connection attempt; a slower node is left out of the report.
	ProbeTimeout = 3 * time.Second
	// Attempts per node; the fastest successful one is reported.
	Attempts = 3
	// MaxTargets bounds one report, matching the server.
	MaxTargets = 64
	// MaxLatency is the server's upper bound for one measurement.
	MaxLatency = 10 * time.Second

	concurrentProbes = 8
)

// Target is one relay and the host:port to time a connection to.
type Target struct {
	NodeID  string
	Address string
}

// Measurement is the fastest successful connect time to a node, in milliseconds (at least 1).
type Measurement struct {
	NodeID    string `json:"node_id"`
	RTTMillis int64  `json:"rtt_ms"`
}

// Dial opens a connection to address; the default is a plain TCP connect.
type Dial func(ctx context.Context, address string) error

func tcpDial(ctx context.Context, address string) error {
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	return connection.Close()
}

// Probe times Attempts connections to every target and returns the fastest success per node,
// fastest first. Nodes that never answered within ProbeTimeout are absent.
func Probe(ctx context.Context, targets []Target, dial Dial) []Measurement {
	if dial == nil {
		dial = tcpDial
	}
	if len(targets) > MaxTargets {
		targets = targets[:MaxTargets]
	}
	var (
		mu      sync.Mutex
		results = make([]Measurement, 0, len(targets))
		wg      sync.WaitGroup
		slots   = make(chan struct{}, concurrentProbes)
	)
	for _, target := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			best := time.Duration(0)
			for range Attempts {
				attempt, cancel := context.WithTimeout(ctx, ProbeTimeout)
				started := time.Now()
				err := dial(attempt, target.Address)
				elapsed := time.Since(started)
				cancel()
				if err == nil && elapsed <= MaxLatency && (best == 0 || elapsed < best) {
					best = elapsed
				}
				if ctx.Err() != nil {
					return
				}
			}
			if best == 0 {
				return
			}
			mu.Lock()
			results = append(results, Measurement{NodeID: target.NodeID, RTTMillis: max(1, best.Milliseconds())})
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(results, func(i, j int) bool {
		if results[i].RTTMillis != results[j].RTTMillis {
			return results[i].RTTMillis < results[j].RTTMillis
		}
		return results[i].NodeID < results[j].NodeID
	})
	return results
}

// Reporter probes and reports on a schedule until its context ends. A failed round is passed to
// OnFailure and retried at the next interval; it never stops the daemon.
type Reporter struct {
	Targets   func(context.Context) ([]Target, error)
	Report    func(context.Context, []Measurement) error
	OnFailure func(stage string, err error)
	Dial      Dial
	First     time.Duration // zero means FirstReportDelay
	Every     time.Duration // zero means Interval
}

func jittered(d time.Duration) time.Duration {
	return d - d/5 + time.Duration(rand.Int64N(int64(d)*2/5+1))
}

// Run blocks until ctx ends.
func (r *Reporter) Run(ctx context.Context) {
	wait := r.First
	if wait == 0 {
		wait = FirstReportDelay
	}
	every := r.Every
	if every == 0 {
		every = Interval
	}
	for {
		timer := time.NewTimer(jittered(wait))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		r.once(ctx)
		wait = every
	}
}

func (r *Reporter) once(ctx context.Context) {
	fail := func(stage string, err error) {
		if r.OnFailure != nil && ctx.Err() == nil {
			r.OnFailure(stage, err)
		}
	}
	targets, err := r.Targets(ctx)
	if err != nil {
		fail("targets", err)
		return
	}
	if len(targets) == 0 {
		return
	}
	measurements := Probe(ctx, targets, r.Dial)
	if len(measurements) == 0 {
		fail("probe", errors.New("no relay answered"))
		return
	}
	if err := r.Report(ctx, measurements); err != nil {
		fail("report", err)
	}
}
