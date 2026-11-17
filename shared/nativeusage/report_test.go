package nativeusage

import (
	"encoding/json"
	"testing"
	"time"
)

// The machine proof covers these bytes: a reorder or a retag breaks every report.
func TestBatchWire(t *testing.T) {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	raw, err := json.Marshal(Batch{Reports: []Report{{AccessSessionID: "a", StreamID: "s", Consumer: "c", Mode: "direct", IntervalStart: at, IntervalEnd: at.Add(time.Minute), UploadBytes: 1, DownloadBytes: 2}}})
	want := `{"reports":[{"access_session_id":"a","stream_id":"s","consumer":"c","mode":"direct","node_id":"","interval_start":"2030-01-01T00:00:00Z","interval_end":"2030-01-01T00:01:00Z","upload_bytes":1,"download_bytes":2}],"unrecorded_bytes":0,"expired_bytes":0,"recording_incomplete":false}`
	if err != nil || string(raw) != want {
		t.Fatalf("batch = %s, %v", raw, err)
	}
}
