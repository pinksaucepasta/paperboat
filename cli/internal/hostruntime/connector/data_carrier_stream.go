package connector

import (
	"sync"
	"time"

	sharedprotocol "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

// DataCarrierStream owns one stream permit.  Closing it is required even
// after receiving EOF so the carrier can admit another stream.
type DataCarrierStream struct {
	edgeTarget DataCarrierTarget
	raw        DataCarrierStreamLink
	Open       StreamOpen
	release    func()
	onClose    func()
	cancelMu   sync.Mutex
	stopCancel func() bool
	closeOnce  sync.Once
	closeErr   error
	// edgeDraining, set by the pool, takes this stream's carrier out of new-stream use.
	edgeDraining func()
}

// goawayReadLimit bounds reading the one notice of a goaway stream.
const goawayReadLimit = 2 * time.Second

// HandleGoaway takes an edge-opened stream whose preface said Kind goaway: it reads the drain
// notice and closes the stream (the edge waits for that). An edge_draining notice moves the pool's
// new streams to a replacement carrier; any other or malformed notice changes nothing.
func (s *DataCarrierStream) HandleGoaway() {
	defer s.Close()
	if s.SetReadDeadline(time.Now().Add(goawayReadLimit)) != nil {
		return
	}
	notice, err := sharedprotocol.ReadGoawayNotice(s)
	if err == nil && notice.Reason == sharedprotocol.GoawayEdgeDraining && s.edgeDraining != nil {
		s.edgeDraining()
	}
}

// EdgeTarget is the authenticated endpoint selected for the accepting carrier.
func (s *DataCarrierStream) EdgeTarget() DataCarrierTarget { return s.edgeTarget }

func (s *DataCarrierStream) Read(p []byte) (int, error) {
	if s == nil || s.raw == nil {
		return 0, ErrDataCarrierClosed
	}
	return s.raw.Read(p)
}

func (s *DataCarrierStream) Write(p []byte) (int, error) {
	if s == nil || s.raw == nil {
		return 0, ErrDataCarrierClosed
	}
	return s.raw.Write(p)
}

func (s *DataCarrierStream) Close() error {
	if s == nil || s.raw == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.cancelMu.Lock()
		stopCancel := s.stopCancel
		s.cancelMu.Unlock()
		if stopCancel != nil {
			stopCancel()
		}
		s.closeErr = s.raw.Close()
		if s.release != nil {
			s.release()
		}
		if s.onClose != nil {
			s.onClose()
		}
	})
	return s.closeErr
}

// CloseWrite sends the native stream FIN while retaining the stream permit and
// cancellation tracking until Close releases the complete stream.
func (s *DataCarrierStream) CloseWrite() error {
	if s == nil || s.raw == nil {
		return ErrDataCarrierClosed
	}
	if closer, ok := s.raw.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return s.raw.Close()
}

func (s *DataCarrierStream) abort() error {
	if s == nil || s.raw == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if resetter, ok := s.raw.(interface{ Reset() error }); ok {
			s.closeErr = resetter.Reset()
		} else {
			s.closeErr = s.raw.Close()
		}
		if s.release != nil {
			s.release()
		}
		if s.onClose != nil {
			s.onClose()
		}
	})
	return s.closeErr
}

func (s *DataCarrierStream) StreamID() uint32 {
	if s == nil || s.raw == nil {
		return 0
	}
	if identified, ok := s.raw.(interface{ StreamID() uint32 }); ok {
		return identified.StreamID()
	}
	return 0
}

func (s *DataCarrierStream) SetDeadline(deadline time.Time) error {
	if s == nil || s.raw == nil {
		return ErrDataCarrierClosed
	}
	return s.raw.SetDeadline(deadline)
}

func (s *DataCarrierStream) SetReadDeadline(deadline time.Time) error {
	if s == nil || s.raw == nil {
		return ErrDataCarrierClosed
	}
	return s.raw.SetReadDeadline(deadline)
}

func (s *DataCarrierStream) SetWriteDeadline(deadline time.Time) error {
	if s == nil || s.raw == nil {
		return ErrDataCarrierClosed
	}
	return s.raw.SetWriteDeadline(deadline)
}
