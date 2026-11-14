package native

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/shared/reattach"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
	"github.com/quic-go/quic-go"
)

// Like EternalTerminal ServerConnection.cpp RETRY_LATER, refuse admission
// explicitly. QUIC stream resets preserve the established peer connection.
const (
	AdmissionDenied      quic.StreamErrorCode = 0x5043
	AdmissionUnavailable quic.StreamErrorCode = 0x5044
)

type AdmissionRefusal struct {
	Code quic.StreamErrorCode
	Err  error
}

func (e *AdmissionRefusal) Error() string {
	if e.Code == AdmissionUnavailable {
		return "native application admission is temporarily unavailable"
	}
	return "native application admission was denied"
}

func (e *AdmissionRefusal) DiagnosticStatus() int {
	if e.Code == AdmissionUnavailable {
		return http.StatusServiceUnavailable
	}
	return http.StatusForbidden
}

func (e *AdmissionRefusal) Unwrap() error        { return e.Err }
func (e *AdmissionRefusal) Is(target error) bool { return target == tailnet.ErrAdmission }

// AdmissionRetryInterval bounds retries inside one application admission.
const AdmissionRetryInterval = 100 * time.Millisecond

// The success byte keeps application bytes behind admission. A refusal uses
// QUIC's typed stream reset; retrying it cannot replay an application request.
func (s *Session) openAdmitted(
	ctx context.Context, header streamauth.Header, encoded []byte,
	resource, capability string,
) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, serverclock.Until(time.Unix(header.DeadlineUnix, 0))) // local timer, server deadline (F21-02)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !s.dialAdmits(resource, capability) {
			return nil, tailnet.ErrAdmission
		}
		connection, err := s.Open(ctx)
		if err != nil {
			return nil, err
		}
		end := diagnostics.ReconnectStep(ctx, "admission_ack")
		err = writeHeader(connection, encoded)
		if err == nil {
			err = readAdmissionAck(ctx, connection)
		}
		var reset *quic.StreamError
		if errors.As(err, &reset) && reset.Remote && (reset.ErrorCode == AdmissionDenied || reset.ErrorCode == AdmissionUnavailable) {
			err = &AdmissionRefusal{Code: reset.ErrorCode, Err: err}
		}
		end(err)
		if err == nil {
			return s.bindScope(newLimitedConn(connection, header.MaximumBytes), resource, capability, "dial", time.Unix(header.DeadlineUnix, 0))
		}
		if stream, ok := connection.(interface{ CancelRead(quic.StreamErrorCode) }); ok {
			stream.CancelRead(0)
		}
		_ = connection.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var refusal *AdmissionRefusal
		if !errors.As(err, &refusal) || refusal.Code != AdmissionUnavailable {
			return nil, err
		}
		if err := reattach.Wait(ctx, AdmissionRetryInterval); err != nil {
			return nil, err
		}
	}
}

func readAdmissionAck(ctx context.Context, connection net.Conn) error {
	deadline, _ := ctx.Deadline()
	if err := connection.SetReadDeadline(deadline); err != nil {
		return err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = connection.SetReadDeadline(time.Now())
		close(done)
	})
	var ack [1]byte
	_, err := io.ReadFull(connection, ack[:])
	if !stop() {
		<-done
	}
	err = errors.Join(err, connection.SetReadDeadline(time.Time{}))
	if err == nil && ack[0] != 0 {
		return streamauth.ErrInvalid
	}
	return err
}

func acknowledgeAdmission(connection net.Conn) error {
	if err := connection.SetWriteDeadline(time.Now().Add(authorizationHeaderTimeout)); err != nil {
		return err
	}
	return errors.Join(writeFull(connection, []byte{0}), connection.SetWriteDeadline(time.Time{}))
}
