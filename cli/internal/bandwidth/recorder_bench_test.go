package bandwidth

import (
	"testing"
	"time"
)

// Every metered read and write records its bytes; the counter key was encoded and hashed each time.
func BenchmarkRecorderAdd(b *testing.B) {
	r := newTestRecorder(b, &testSink{})
	binding := Binding{AccessSessionID: "access", StreamID: "stream", Consumer: "terminal"}
	now := time.Now().UTC()
	b.ReportAllocs()
	for b.Loop() {
		r.add(binding, Path{Mode: "direct"}, 4096, true, now)
	}
}
