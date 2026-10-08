// Package edgeplacement chooses the machine's preferred web edge locations from
// bounded connection measurements. The control plane authorizes the result.
package edgeplacement

import (
	"context"
	"errors"
	"net"
	"sort"
	"sync"
	"time"
)

const MaximumCandidates = 32

var ErrInvalid = errors.New("invalid edge placement candidates")

type Candidate struct {
	NodeID        string `json:"node_id"`
	ProcessEpoch  string `json:"process_epoch"`
	Region        string `json:"region"`
	FailureDomain string `json:"failure_domain"`
	ProbeAddress  string `json:"probe_address"`
}

type Preference struct {
	PrimaryNodeID         string `json:"primary_node_id"`
	PrimaryProcessEpoch   string `json:"primary_process_epoch"`
	SecondaryNodeID       string `json:"secondary_node_id,omitempty"`
	SecondaryProcessEpoch string `json:"secondary_process_epoch,omitempty"`
}

type Measurement struct {
	Candidate Candidate
	RTT       time.Duration
}

type Selector struct {
	current     Preference
	candidate   string
	first       time.Time
	last        time.Time
	consecutive int
}

// Select keeps a healthy primary until another location is materially faster
// on three spaced measurements. Loss of the primary or its process epoch
// switches immediately. A backup must have a separate failure domain.
func (s *Selector) Select(now time.Time, measurements []Measurement) (Preference, error) {
	if s == nil || now.IsZero() || len(measurements) == 0 || len(measurements) > MaximumCandidates {
		return Preference{}, ErrInvalid
	}
	ordered := make([]Measurement, 0, len(measurements))
	seen := make(map[string]bool, len(measurements))
	for _, m := range measurements {
		key := m.Candidate.NodeID + "\x00" + m.Candidate.ProcessEpoch
		if m.Candidate.NodeID == "" || m.Candidate.ProcessEpoch == "" || m.RTT <= 0 || m.RTT > time.Second*10 || seen[key] {
			return Preference{}, ErrInvalid
		}
		seen[key] = true
		ordered = append(ordered, m)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].RTT == ordered[j].RTT {
			return ordered[i].Candidate.NodeID < ordered[j].Candidate.NodeID
		}
		return ordered[i].RTT < ordered[j].RTT
	})
	chosen := ordered[0]
	currentIndex := -1
	for i, m := range ordered {
		if m.Candidate.NodeID == s.current.PrimaryNodeID && m.Candidate.ProcessEpoch == s.current.PrimaryProcessEpoch {
			currentIndex = i
			break
		}
	}
	if currentIndex >= 0 && currentIndex != 0 {
		current := ordered[currentIndex]
		gain := current.RTT - chosen.RTT
		if gain < 10*time.Millisecond || gain*100 < current.RTT*15 {
			chosen = current
			s.reset()
		} else {
			key := ordered[0].Candidate.NodeID + "\x00" + ordered[0].Candidate.ProcessEpoch
			if s.candidate != key || now.Sub(s.last) < 10*time.Second || now.Sub(s.last) > time.Minute {
				s.candidate, s.first, s.last, s.consecutive = key, now, now, 1
				chosen = current
			} else {
				s.last = now
				s.consecutive++
				if s.consecutive < 3 || now.Sub(s.first) < 20*time.Second {
					chosen = current
				} else {
					s.reset()
				}
			}
		}
	} else {
		s.reset()
	}
	pref := Preference{PrimaryNodeID: chosen.Candidate.NodeID, PrimaryProcessEpoch: chosen.Candidate.ProcessEpoch}
	for _, m := range ordered {
		if m.Candidate.NodeID == chosen.Candidate.NodeID || chosen.Candidate.FailureDomain == "" || m.Candidate.FailureDomain == "" || m.Candidate.FailureDomain == chosen.Candidate.FailureDomain {
			continue
		}
		pref.SecondaryNodeID, pref.SecondaryProcessEpoch = m.Candidate.NodeID, m.Candidate.ProcessEpoch
		break
	}
	s.current = pref
	return pref, nil
}

func (s *Selector) reset() {
	s.candidate = ""
	s.first = time.Time{}
	s.last = time.Time{}
	s.consecutive = 0
}

// Probe measures the machine's TCP setup time to the published carrier port.
// A later authenticated carrier handshake still verifies the exact edge key.
// Eight concurrent one-second probes bound work even with 32 candidates.
func Probe(ctx context.Context, candidates []Candidate, dial func(context.Context, string) error) ([]Measurement, error) {
	if ctx == nil || len(candidates) == 0 || len(candidates) > MaximumCandidates {
		return nil, ErrInvalid
	}
	if dial == nil {
		dialer := &net.Dialer{Timeout: time.Second}
		dial = func(ctx context.Context, address string) error {
			connection, err := dialer.DialContext(ctx, "tcp", address)
			if err == nil {
				_ = connection.Close()
			}
			return err
		}
	}
	var mu sync.Mutex
	var wait sync.WaitGroup
	semaphore := make(chan struct{}, 8)
	measurements := make([]Measurement, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.NodeID == "" || candidate.ProcessEpoch == "" {
			return nil, ErrInvalid
		}
		if _, _, err := net.SplitHostPort(candidate.ProbeAddress); err != nil {
			return nil, ErrInvalid
		}
	}
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			break
		}
		semaphore <- struct{}{}
		wait.Add(1)
		go func(candidate Candidate) {
			defer wait.Done()
			defer func() { <-semaphore }()
			probeCtx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			start := time.Now()
			if dial(probeCtx, candidate.ProbeAddress) != nil || probeCtx.Err() != nil {
				return
			}
			rtt := time.Since(start)
			if rtt <= 0 {
				rtt = time.Microsecond
			}
			mu.Lock()
			measurements = append(measurements, Measurement{Candidate: candidate, RTT: rtt})
			mu.Unlock()
		}(candidate)
	}
	wait.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return measurements, nil
}
