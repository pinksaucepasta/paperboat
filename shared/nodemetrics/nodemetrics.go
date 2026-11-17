// Package nodemetrics is the one definition of what an edge or relay reports about itself in its heartbeat to the
// control plane (version, CPU, memory, bytes carried) and the one reader of this process's CPU and memory. The
// control plane stores the latest report and daily byte totals per node; the operator panel shows them with warnings.
package nodemetrics

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Report is sent with every heartbeat. Bytes are cumulative since the process started (its process epoch), so the
// control plane can sum them into days without losing a heartbeat; a restart starts a new epoch.
type Report struct {
	// Version is the software release of the process.
	Version string `json:"version,omitempty"`
	// CPUPercent is the process's CPU use over the last sample, as a share of all the host's CPUs (0–100).
	CPUPercent float64 `json:"cpu_percent"`
	// RSSBytes is the process's resident memory.
	RSSBytes uint64 `json:"rss_bytes"`
	// BytesIn and BytesOut are what the node received and sent (an edge: ingress and egress; a relay: forwarded
	// payload bytes, counted in BytesOut only).
	BytesIn  uint64 `json:"bytes_in"`
	BytesOut uint64 `json:"bytes_out"`
}

const (
	MaxVersionLength = 64
	// MaxBytes bounds every byte counter a node may claim (1 EiB), so a corrupt report cannot overflow a total.
	MaxBytes = 1 << 60
	maxRSS   = 1 << 50
)

// Validate bounds a report received from a node.
func (r Report) Validate() error {
	switch {
	case len(r.Version) > MaxVersionLength || strings.ContainsAny(r.Version, "\x00\r\n"):
		return errors.New("version is too long or has control characters")
	case math.IsNaN(r.CPUPercent) || r.CPUPercent < 0 || r.CPUPercent > 100:
		return errors.New("cpu_percent must be between 0 and 100")
	case r.RSSBytes > maxRSS:
		return errors.New("rss_bytes is out of range")
	case r.BytesIn > MaxBytes || r.BytesOut > MaxBytes:
		return errors.New("a byte counter is out of range")
	}
	return nil
}

// Sampler reads this process's CPU and memory. CPU is the change since the previous Sample, so the first call
// reports 0.
type Sampler struct {
	mu       sync.Mutex
	lastCPU  float64 // CPU seconds used at the previous sample
	lastAt   time.Time
	hasLast  bool
	numCPU   int
	readProc func() (cpuSeconds float64, rss uint64, err error)
}

// NewSampler returns a sampler for this process.
func NewSampler() *Sampler { return &Sampler{numCPU: cpuCount(), readProc: readProcess} }

// Sample returns the CPU share (0–100 of all CPUs) since the previous call and the resident memory now. When the
// platform gives no process statistics both are 0.
func (s *Sampler) Sample(now time.Time) (cpuPercent float64, rss uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seconds, rss, err := s.readProc()
	if err != nil {
		return 0, 0
	}
	if s.hasLast {
		if wall := now.Sub(s.lastAt).Seconds(); wall > 0 && seconds >= s.lastCPU {
			cpuPercent = math.Min(100, (seconds-s.lastCPU)/(wall*float64(s.numCPU))*100)
		}
	}
	s.lastCPU, s.lastAt, s.hasLast = seconds, now, true
	return cpuPercent, rss
}

// parseStat returns utime+stime in clock ticks from the contents of /proc/<pid>/stat. The command name is in
// parentheses and may hold spaces, so fields are counted after the last ")".
func parseStat(stat string) (uint64, error) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, errors.New("malformed stat")
	}
	fields := strings.Fields(stat[end+1:])
	// fields[0] is field 3 (state); utime is field 14 and stime field 15.
	if len(fields) < 13 {
		return 0, errors.New("short stat")
	}
	user, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return 0, err
	}
	system, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return 0, err
	}
	return user + system, nil
}

// parseStatm returns the resident set in pages from the contents of /proc/<pid>/statm (its second field).
func parseStatm(statm string) (uint64, error) {
	fields := strings.Fields(statm)
	if len(fields) < 2 {
		return 0, errors.New("short statm")
	}
	return strconv.ParseUint(fields[1], 10, 64)
}
