package tunnel

import "sync"

// RecordingObserver is a terminal connection that reports whether its session is being recorded by
// the team (REC). A late observer is told the current state at once.
type RecordingObserver interface{ ObserveRecording(func(recording bool)) }

// recordingState keeps the last REC state of one link and tells its observer of every change.
type recordingState struct {
	mu        sync.Mutex
	known     bool
	recording bool
	observer  func(bool)
}

func (s *recordingState) report(recording bool) {
	s.mu.Lock()
	changed := !s.known || s.recording != recording
	s.known, s.recording = true, recording
	observer := s.observer
	s.mu.Unlock()
	if changed && observer != nil {
		observer(recording)
	}
}

func (s *recordingState) observe(observer func(bool)) {
	s.mu.Lock()
	s.observer = observer
	known, recording := s.known, s.recording
	s.mu.Unlock()
	if known {
		observer(recording)
	}
}
