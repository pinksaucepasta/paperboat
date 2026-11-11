package tunnel

import (
	"slices"
	"sync"
)

// RecordingObserver is a terminal connection that reports whether its session is being recorded by
// the team (REC). A late observer is told the current state at once.
type RecordingObserver interface{ ObserveRecording(func(recording bool)) }

// recordingState keeps the last REC state of one link and tells every observer of every change, in the order they
// registered. A link has more than one: the reconnect supervisor wrapped around it and the local API serving it.
type recordingState struct {
	mu        sync.Mutex
	known     bool
	recording bool
	observers []func(bool)
}

func (s *recordingState) report(recording bool) {
	s.mu.Lock()
	changed := !s.known || s.recording != recording
	s.known, s.recording = true, recording
	observers := slices.Clone(s.observers)
	s.mu.Unlock()
	if changed {
		for _, observer := range observers {
			observer(recording)
		}
	}
}

func (s *recordingState) observe(observer func(bool)) {
	s.mu.Lock()
	s.observers = append(s.observers, observer)
	known, recording := s.known, s.recording
	s.mu.Unlock()
	if known {
		observer(recording)
	}
}
