package recordings

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/recording"
)

type event struct {
	at   time.Time
	code string
	data []byte
}

type stopRequest struct{ actor, reason string }

// recorder owns one recording. Only its goroutine touches buffer, carry and the segment chain; mu
// guards meta, which State, Join and List read.
type recorder struct {
	service *Service
	dataKey []byte
	sealed  recording.SealedKey
	quota   int64

	mu   sync.Mutex
	meta Meta

	events  chan event
	queued  atomic.Int64
	dropped atomic.Int64
	stop    chan stopRequest
	ended   chan struct{}
	result  Meta

	buffer   bytes.Buffer
	carry    []byte // an incomplete UTF-8 character at the end of the last output
	opened   time.Time
	previous string
	elapsed  time.Duration
}

func newRecorder(s *Service, p *Prepared, header []byte) *recorder {
	r := &recorder{service: s, dataKey: p.dataKey, sealed: p.sealed, quota: p.quota, meta: p.meta,
		events: make(chan event, 4096), stop: make(chan stopRequest, 1), ended: make(chan struct{}), opened: p.meta.StartedAt}
	r.buffer.Write(header)
	return r
}

func (r *recorder) state() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return State{ID: r.meta.ID, Trigger: r.meta.Trigger, StartedBy: r.meta.StartedBy, StartedAt: r.meta.StartedAt}
}

func (r *recorder) snapshot() Meta {
	r.mu.Lock()
	defer r.mu.Unlock()
	meta := r.meta
	meta.Accounts = append([]string(nil), meta.Accounts...)
	return meta
}

// enqueue copies data into the bounded queue or counts it as dropped; it never waits.
func (r *recorder) enqueue(e event) {
	size := int64(len(e.data))
	if r.queued.Add(size) > r.service.config.QueueBytes {
		r.queued.Add(-size)
		r.dropped.Add(size)
		return
	}
	e.data = bytes.Clone(e.data)
	select {
	case r.events <- e:
	default:
		r.queued.Add(-size)
		r.dropped.Add(size)
	}
}

func (r *recorder) run() {
	ticker := time.NewTicker(r.service.config.SegmentAge)
	defer ticker.Stop()
	for {
		select {
		case e := <-r.events:
			r.queued.Add(-int64(len(e.data)))
			r.add(e)
			if r.buffer.Len() >= r.service.config.SegmentBytes {
				if reason := r.flush(false, ""); reason != "" {
					r.end("", reason)
					return
				}
			}
		case <-ticker.C:
			if r.buffer.Len() > 0 {
				if reason := r.flush(false, ""); reason != "" {
					r.end("", reason)
					return
				}
			}
		case request := <-r.stop:
			r.drain()
			reason := request.reason
			if failure := r.flush(true, request.reason); failure != "" {
				reason = failure
			}
			r.end(request.actor, reason)
			return
		}
	}
}

// drain takes whatever output was queued before the stop.
func (r *recorder) drain() {
	for {
		select {
		case e := <-r.events:
			r.queued.Add(-int64(len(e.data)))
			r.add(e)
		default:
			return
		}
	}
}

func (r *recorder) add(e event) {
	if lost := r.dropped.Swap(0); lost > 0 {
		r.line(e.at, recording.CodeMarker, fmt.Sprintf("gap: %d bytes of output were not recorded", lost))
	}
	if e.code == recording.CodeOutput {
		r.appendOutput(e.at, e.data)
		return
	}
	r.line(e.at, e.code, string(e.data))
}

func (r *recorder) appendOutput(at time.Time, data []byte) {
	data = append(r.carry, data...)
	complete, rest := splitUTF8(data)
	r.carry = append([]byte(nil), rest...)
	if len(complete) > 0 {
		r.line(at, recording.CodeOutput, string(complete))
	}
}

// splitUTF8 holds back a character split across two PTY reads, so it is not recorded as U+FFFD.
func splitUTF8(b []byte) (complete, rest []byte) {
	for i := 1; i <= utf8.UTFMax-1 && i <= len(b); i++ {
		c := b[len(b)-i]
		if c < utf8.RuneSelf {
			break
		}
		if utf8.RuneStart(c) {
			if !utf8.FullRune(b[len(b)-i:]) {
				return b[:len(b)-i], b[len(b)-i:]
			}
			break
		}
	}
	return b, nil
}

func (r *recorder) line(at time.Time, code, data string) {
	if elapsed := at.Sub(r.opened); elapsed > r.elapsed {
		r.elapsed = elapsed
	}
	r.buffer.Write(recording.EventLine(r.elapsed, code, data))
}

// flush writes the buffer as the next segment. It returns an end reason when the recording cannot
// continue: the quota is full of active recordings, or the disk failed.
func (r *recorder) flush(final bool, endReason string) string {
	if r.buffer.Len() == 0 && !final {
		return ""
	}
	r.mu.Lock()
	index, id := r.meta.Segments, r.meta.ID
	generation := r.meta.KeyGeneration
	r.mu.Unlock()
	header := recording.SegmentHeader{Recording: id, Index: index, KeyGeneration: generation, SealedKey: r.sealed, PreviousSHA256: r.previous, Final: final}
	if final {
		header.EndReason = endReason
	}
	file, err := recording.EncodeSegment(header, r.dataKey, r.buffer.Bytes())
	if err != nil {
		return ReasonError
	}
	size := int64(len(file))
	if !r.service.reserve(id, size, r.quota) {
		return ReasonQuota
	}
	if err := atomicfile.Write(filepath.Join(r.service.dir(id), segmentName(index)), file, atomicfile.CurrentOwnerOptions(0o600)); err != nil {
		r.service.release(size)
		return ReasonError
	}
	digest := sha256.Sum256(file)
	r.previous = hex.EncodeToString(digest[:])
	r.buffer.Reset()
	r.mu.Lock()
	r.meta.Segments++
	r.meta.Bytes += size
	r.mu.Unlock()
	return ""
}

// end closes the index entry and reports the recording stopped. It runs once, on the recorder's
// goroutine.
func (r *recorder) end(actor, reason string) {
	now := r.service.config.Now().UTC()
	r.mu.Lock()
	r.meta.EndedAt, r.meta.EndReason, r.meta.StoppedBy = &now, reason, actor
	meta := r.meta
	meta.Accounts = append([]string(nil), meta.Accounts...)
	r.mu.Unlock()
	clear(r.dataKey)
	_ = r.service.writeMeta(meta) // on failure the reporter closes it once its session is gone
	r.service.selfEnded(r)
	r.result = meta
	close(r.ended)
}

// finish asks the goroutine to stop and waits for its last segment.
func (r *recorder) finish(actor, reason string) Meta {
	select {
	case r.stop <- stopRequest{actor: actor, reason: reason}:
	case <-r.ended:
	}
	<-r.ended
	return r.result
}
