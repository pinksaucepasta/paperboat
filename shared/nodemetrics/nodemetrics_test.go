package nodemetrics

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestParseStatCountsFieldsAfterTheCommandName(t *testing.T) {
	// A command name with spaces and a ")" inside, as the kernel writes it.
	stat := "4242 (paper (boat) edge) S 1 4242 4242 0 -1 4194560 1000 0 0 0 150 50 0 0 20 0 8 0 100 1000000 500 18446744073709551615"
	got, err := parseStat(stat)
	if err != nil || got != 200 {
		t.Fatalf("ticks=%d err=%v", got, err)
	}
	for _, bad := range []string{"", "no parenthesis", "1 (x) S 1 2"} {
		if _, err := parseStat(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if pages, err := parseStatm("5000 1234 300 10 0 900 0"); err != nil || pages != 1234 {
		t.Fatalf("pages=%d err=%v", pages, err)
	}
	if _, err := parseStatm("5000"); err == nil {
		t.Fatal("short statm accepted")
	}
}

func TestSamplerReportsCPUAsAShareOfAllCPUs(t *testing.T) {
	cpu := 10.0
	s := &Sampler{numCPU: 4, readProc: func() (float64, uint64, error) { return cpu, 1 << 20, nil }}
	start := time.Unix(1000, 0)
	if got, rss := s.Sample(start); got != 0 || rss != 1<<20 {
		t.Fatalf("first sample must report no CPU: %v %d", got, rss)
	}
	// 2 CPU-seconds over 4 wall seconds on 4 CPUs is 12.5% of the machine.
	cpu = 12
	if got, _ := s.Sample(start.Add(4 * time.Second)); math.Abs(got-12.5) > 1e-9 {
		t.Fatalf("cpu=%v want 12.5", got)
	}
	// A counter that went backwards (restart of the reader) is not a negative or huge share.
	cpu = 1
	if got, _ := s.Sample(start.Add(8 * time.Second)); got != 0 {
		t.Fatalf("cpu=%v", got)
	}
}

func TestRealSamplerReadsThisProcessOnLinux(t *testing.T) {
	s := NewSampler()
	_, rss := s.Sample(time.Now())
	if rss == 0 {
		t.Skip("no process statistics on this platform")
	}
	if rss < 1<<20 {
		t.Fatalf("a Go test binary cannot be under 1 MiB resident: %d", rss)
	}
}

func TestValidateBoundsEveryField(t *testing.T) {
	ok := Report{Version: "dev-abc", CPUPercent: 12.5, RSSBytes: 1 << 30, BytesIn: 1 << 40, BytesOut: 1 << 40}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]Report{
		"version too long": {Version: strings.Repeat("v", MaxVersionLength+1)},
		"control char":     {Version: "a\nb"},
		"negative cpu":     {CPUPercent: -1},
		"cpu over 100":     {CPUPercent: 100.1},
		"nan cpu":          {CPUPercent: math.NaN()},
		"huge rss":         {RSSBytes: 1 << 51},
		"huge bytes":       {BytesOut: MaxBytes + 1},
	} {
		if bad.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
