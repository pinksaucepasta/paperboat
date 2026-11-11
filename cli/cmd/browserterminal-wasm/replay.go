package main

// replayRange is a recording-mode connection's paced replay. The machine sends a replay with no output
// window, so the worker reads one segment per request and shows the page one stream: global byte
// offsets from 0 and a single recording_stream_end after the last segment.
type replayRange struct {
	recordingID string
	next        uint32 // the next segment to read
	total       uint32 // segments in the recording when it was first read
	totalBytes  int64
	final       bool
	base        uint64 // the global offset where the current range starts
	length      uint64 // the current range's bytes, segment lengths included
}

// replayReadResult is the machine's answer to one ranged recording.v1 read.
type replayReadResult struct {
	RecordingID   string `json:"recording_id"`
	StreamID      uint32 `json:"stream_id"`
	FromSegment   uint32 `json:"from_segment"`
	Segments      uint32 `json:"segments"`
	TotalSegments uint32 `json:"total_segments"`
	Bytes         int64  `json:"bytes"`
	TotalBytes    int64  `json:"total_bytes"`
	Final         bool   `json:"final"`
}

// bind takes one range's answer; the first fixes the recording's size. An answer that does not
// continue the replay (another recording, a gap, an empty range before the end) is refused.
func (r *replayRange) bind(result replayReadResult, first bool) bool {
	if result.RecordingID != r.recordingID || result.StreamID == 0 || result.FromSegment != r.next || result.Segments > 1 || result.Bytes < 0 {
		return false
	}
	if first {
		if result.TotalBytes < 0 || result.TotalSegments < result.Segments {
			return false
		}
		r.total, r.totalBytes = result.TotalSegments, result.TotalBytes
	} else if result.Segments == 0 {
		return false
	}
	r.next, r.length, r.final = result.FromSegment+result.Segments, uint64(result.Bytes)+4*uint64(result.Segments), result.Final
	return true
}

// ended moves past a finished range and says which segment to read next, if any.
func (r *replayRange) ended() (from uint32, more bool) {
	r.base, r.length = r.base+r.length, 0
	return r.next, r.next < r.total
}

// streamBytes is what the page receives in all: every segment behind its 4-byte length.
func (r *replayRange) streamBytes() int64 { return r.totalBytes + 4*int64(r.total) }
