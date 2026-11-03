package native

import (
	"context"
	"encoding/binary"
	"io"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/quic-go/quic-go"
)

// OnCarrierMove installs the terminal's move handler on its authenticated old
// connection. The reserved candidate stream never enters application output.
func (s *Session) OnCarrierMove(move func(context.Context, CarrierOffer) error) error {
	if s == nil || move == nil {
		return ErrInvalid
	}
	s.routerMu.Lock()
	defer s.routerMu.Unlock()
	if s.moveCarrier != nil {
		return ErrInvalid
	}
	if s.router == nil {
		config := peerquic.DevelopmentStreamRouterConfig()
		config.CandidateControl = s.receiveCarrierOffer
		router, err := peerquic.NewStreamRouter(s.quic, config)
		if err != nil {
			return err
		}
		s.router = router
	}
	s.moveCarrier = move
	return nil
}

// OfferCarrier waits for the client to finish moving, not merely to receive an
// address. Failure leaves the old connection serving; the updater can reconnect.
func (s *Session) OfferCarrier(ctx context.Context, offer CarrierOffer) error {
	if s == nil || ctx == nil {
		return ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	self, err := s.owner.authority.Self()
	if err != nil || offer.verify(self, time.Now()) != nil {
		return ErrInvalid
	}
	body, err := offer.MarshalBinary()
	if err != nil {
		return err
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(body)))
	stream, err := s.quic.OpenCandidateControl(ctx, nil)
	if err != nil {
		return err
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() {
		if q, ok := stream.(*quic.Stream); ok {
			q.CancelRead(0)
			q.CancelWrite(0)
		}
	})
	defer stop()
	if _, err = stream.Write(append(size[:], body...)); err != nil {
		return err
	}
	var result [1]byte
	if _, err := io.ReadFull(stream, result[:]); err != nil {
		return err
	}
	if result[0] != 1 {
		return ErrStreamRejected
	}
	return nil
}

func (s *Session) receiveCarrierOffer(ctx context.Context, stream *quic.Stream) error {
	defer stream.Close()
	// Both ends have the updater's bounded preparation lifetime. A peer cannot
	// keep a classifier or an old generation alive indefinitely.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { stream.CancelRead(0); stream.CancelWrite(0) })
	defer stop()
	var size [4]byte
	if _, err := io.ReadFull(stream, size[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > maximumCarrierOffer {
		return ErrInvalid
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(stream, body); err != nil {
		return err
	}
	offer, err := ParseCarrierOffer(body)
	if err != nil {
		return err
	}
	parent, err := s.owner.authority.Peer(s.peerID)
	if err != nil || parent != s.binding || offer.verify(parent, time.Now()) != nil {
		return ErrInvalid
	}
	s.routerMu.Lock()
	move := s.moveCarrier
	s.routerMu.Unlock()
	result := byte(0)
	if move != nil && move(ctx, offer) == nil {
		result = 1
	}
	_, err = stream.Write([]byte{result})
	return err
}

// OfferCarrier moves terminal connections before the old process drains.
// Other applications retain their connection for the bounded retirement drain.
func (o *Owner) OfferCarrier(ctx context.Context, offer CarrierOffer) error {
	if o == nil || ctx == nil {
		return ErrInvalid
	}
	o.mu.Lock()
	sessions := make([]*Session, 0, len(o.sessions))
	for session := range o.sessions {
		if session.terminalUsed.Load() {
			sessions = append(sessions, session)
		}
	}
	o.mu.Unlock()
	results := make(chan error, len(sessions))
	for _, session := range sessions {
		go func() { results <- session.OfferCarrier(ctx, offer) }()
	}
	var result error
	for range sessions {
		select {
		case err := <-results:
			if err != nil && result == nil {
				result = err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return result
}
