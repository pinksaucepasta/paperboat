package native

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"github.com/quic-go/quic-go"
)

// moveErrorType names a move failure with bounded metadata only.
func moveErrorType(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrStreamRejected):
		return "rejected"
	case errors.Is(err, ErrInvalid):
		return "invalid"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	}
	return strings.TrimPrefix(fmt.Sprintf("%T", err), "*")
}

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
	if err := s.ensureRouterLocked(); err != nil {
		return err
	}
	s.moveCarrier = move
	return nil
}

// CarrierMoveBudget bounds one move on both ends. The old connection keeps
// serving until the replacement is attached, so a slow cold relay (dadape
// relay1d: over 9 s) lengthens the update, not the pause. It stays under the
// session owner's 30 s RPC bound that carries the previous generation's move.
const CarrierMoveBudget = 25 * time.Second

// OfferCarrier waits for the client to finish moving, not merely to receive an
// address. Failure leaves the old connection serving; the updater can reconnect.
func (s *Session) OfferCarrier(ctx context.Context, offer CarrierOffer) error {
	if s == nil || ctx == nil {
		return ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, CarrierMoveBudget)
	defer cancel()
	self, err := s.owner.authority.Self()
	if err != nil || offer.verify(self, time.Now()) != nil {
		return ErrInvalid
	}
	body, err := json.Marshal(carrierControl{Kind: "offer", Exchange: CarrierExchange{Offer: offer}})
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

func (s *Session) receiveCarrierControl(ctx context.Context, stream *quic.Stream) error {
	defer stream.Close()
	// Both ends have the updater's bounded preparation lifetime. A peer cannot
	// keep a classifier or an old generation alive indefinitely.
	ctx, cancel := context.WithTimeout(ctx, CarrierMoveBudget)
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
	var request carrierControl
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		return ErrInvalid
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return ErrInvalid
	}
	if request.Kind == "candidates" {
		return s.receiveCarrierCandidates(ctx, stream, request.Exchange)
	}
	if request.Kind != "offer" {
		return ErrInvalid
	}
	offer := request.Exchange.Offer
	parent, err := s.owner.authority.Peer(s.peerID)
	if err != nil || parent != s.binding || offer.verify(parent, time.Now()) != nil {
		return ErrInvalid
	}
	s.routerMu.Lock()
	move := s.moveCarrier
	s.routerMu.Unlock()
	result := byte(0)
	moveErr := ErrInvalid
	// A generation that serves every application takes this client's new
	// connections; its open streams, terminals included, stay here (a terminal
	// moves with the session owner's own handover).
	switch {
	case offer.Claims.AllApplications && s.owner.hasRoute(s.peerID, offer):
		moveErr = nil
	case offer.Claims.AllApplications:
		// Connect the generation once so this client's new connections open on
		// it; this connection keeps its open streams.
		var next *Session
		if next, moveErr = s.DialCarrier(ctx, offer); moveErr == nil {
			next.Close()
		}
	case move != nil:
		moveErr = move(ctx, offer)
	}
	if moveErr == nil {
		result = 1
	}
	diagnosticlog.TryInfo("terminal carrier move", "stage", "offer_received", "failed", moveErr != nil, "error_type", moveErrorType(moveErr))
	_, err = stream.Write([]byte{result})
	return err
}

// OfferCarrier moves terminal connections before the old process drains. A
// generation that serves every application is offered to every connection, so
// each client's new connections open on it; open streams stay for the drain.
func (o *Owner) OfferCarrier(ctx context.Context, offer CarrierOffer) error {
	if o == nil || ctx == nil {
		return ErrInvalid
	}
	o.mu.Lock()
	sessions := make([]*Session, 0, len(o.sessions))
	for session := range o.sessions {
		if session.terminalUsed.Load() || offer.Claims.AllApplications {
			sessions = append(sessions, session)
		}
	}
	o.mu.Unlock()
	diagnosticlog.TryInfo("terminal carrier move", "stage", "offer_start", "sessions", len(sessions))
	results := make(chan error, len(sessions))
	for _, session := range sessions {
		go func() { results <- session.OfferCarrier(ctx, offer) }()
	}
	var result error
wait:
	for range sessions {
		select {
		case err := <-results:
			if err != nil && result == nil {
				result = err
			}
		case <-ctx.Done():
			result = ctx.Err()
			break wait
		}
	}
	diagnosticlog.TryInfo("terminal carrier move", "stage", "offer_end", "failed", result != nil, "error_type", moveErrorType(result))
	return result
}

// CarrierExchange travels only over an already authenticated native connection.
// Peer identity is supplied by the receiving session, never by this payload.
type CarrierExchange struct {
	Offer          CarrierOffer
	RelayNodeID    string
	DiscoPublicKey string
	Endpoints      []string
}
type carrierControl struct {
	Kind     string
	Exchange CarrierExchange
}

func (s *Session) receiveCarrierCandidates(ctx context.Context, stream *quic.Stream, request CarrierExchange) error {
	self, err := s.owner.authority.Self()
	if err != nil || request.Offer.verify(self, time.Now()) != nil || s.owner.prepareCarrier == nil {
		return ErrInvalid
	}
	peer, err := s.owner.authority.Peer(s.peerID)
	if err != nil || peer != s.binding {
		return ErrInvalid
	}
	offer, err := s.owner.prepareCarrier(ctx, s.peerID, request)
	if err != nil {
		return err
	}
	body, err := offer.MarshalBinary()
	if err != nil {
		return err
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(body)))
	_, err = stream.Write(append(size[:], body...))
	return err
}

func (s *Session) exchangeCarrierCandidates(ctx context.Context, request CarrierExchange) (CarrierOffer, error) {
	body, err := json.Marshal(carrierControl{Kind: "candidates", Exchange: request})
	if err != nil || len(body) > maximumCarrierOffer {
		return CarrierOffer{}, ErrInvalid
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(body)))
	stream, err := s.quic.OpenCandidateControl(ctx, append(size[:], body...))
	if err != nil {
		return CarrierOffer{}, err
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() {
		if q, ok := stream.(*quic.Stream); ok {
			q.CancelRead(0)
			q.CancelWrite(0)
		}
	})
	defer stop()
	if _, err := io.ReadFull(stream, size[:]); err != nil {
		return CarrierOffer{}, err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > maximumCarrierOffer {
		return CarrierOffer{}, ErrInvalid
	}
	body = make([]byte, n)
	if _, err := io.ReadFull(stream, body); err != nil {
		return CarrierOffer{}, err
	}
	return ParseCarrierOffer(body)
}
