package nativesession

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
)

type observedHandlerFailure struct {
	cause       error
	errorCalls  int
	stage, code string
}

func (e *observedHandlerFailure) Error() string {
	e.errorCalls++
	return "private payload credential=" + "do-not-retain"
}
func (e *observedHandlerFailure) Unwrap() error           { return e.cause }
func (e *observedHandlerFailure) DiagnosticStage() string { return e.stage }
func (e *observedHandlerFailure) DiagnosticCode() string  { return e.code }

func TestServiceObservesBackgroundHandlerFailuresAndContinues(t *testing.T) {
	for _, test := range []struct {
		name     string
		kind     FailureKind
		consumer string
		stage    string
		code     string
	}{
		{name: "stream", kind: FailureStream, consumer: "terminal", stage: "target_connect", code: "native_private_failed"},
		{name: "transfer", kind: FailureTransfer, consumer: "file_transfer", stage: "delivery", code: "file_transfer_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cause := errors.New("downstream failure")
			failure := &observedHandlerFailure{cause: cause, stage: test.stage, code: test.code}
			handlerErr := error(failure)
			var observed []struct {
				kind FailureKind
				err  error
			}
			service, err := New(Config{
				Authorize:     func(context.Context, streamauth.Header) (string, error) { return "", nil },
				ServeStream:   func(context.Context, streamauth.Header, net.Conn) error { return handlerErr },
				ServeTransfer: func(context.Context, net.Conn) error { return handlerErr },
				ObserveFailure: func(_ context.Context, kind FailureKind, err error) {
					observed = append(observed, struct {
						kind FailureKind
						err  error
					}{kind: kind, err: err})
				},
			})
			if err != nil {
				t.Fatal(err)
			}

			serveOneForTest(t, service, test.consumer)
			if len(observed) != 1 || observed[0].kind != test.kind || observed[0].err != failure {
				t.Fatalf("observed=%+v, want one original %s failure", observed, test.name)
			}
			var typed interface {
				DiagnosticStage() string
				DiagnosticCode() string
			}
			if !errors.As(observed[0].err, &typed) || typed.DiagnosticStage() != test.stage || typed.DiagnosticCode() != test.code || !errors.Is(observed[0].err, cause) {
				t.Fatalf("typed handler failure changed: %T %v", observed[0].err, observed[0].err)
			}
			if failure.errorCalls != 0 {
				t.Fatalf("handler error text was formatted %d times", failure.errorCalls)
			}

			handlerErr = nil
			serveOneForTest(t, service, test.consumer)
			if len(observed) != 1 {
				t.Fatalf("successful follow-up changed failure observations: %+v", observed)
			}
		})
	}
}

func TestServiceSuppressesNormalHandlerTerminationAndKeepsJoinedFailure(t *testing.T) {
	var observed []error
	service, err := New(Config{
		Authorize:     func(context.Context, streamauth.Header) (string, error) { return "", nil },
		ServeStream:   func(context.Context, streamauth.Header, net.Conn) error { return nil },
		ServeTransfer: func(context.Context, net.Conn) error { return nil },
		ObserveFailure: func(_ context.Context, _ FailureKind, err error) {
			observed = append(observed, err)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, normal := range []error{
		io.EOF,
		net.ErrClosed,
		context.Canceled,
		errors.Join(io.EOF, net.ErrClosed, context.Canceled),
	} {
		service.config.ServeStream = func(context.Context, streamauth.Header, net.Conn) error { return normal }
		serveOneForTest(t, service, "terminal")
	}
	if len(observed) != 0 {
		t.Fatalf("normal stream termination produced observations: %+v", observed)
	}

	cause := errors.New("authorization worker failed while stopping")
	joined := errors.Join(context.Canceled, cause)
	service.config.ServeTransfer = func(context.Context, net.Conn) error { return joined }
	serveOneForTest(t, service, "file_transfer")
	if len(observed) != 1 || observed[0] != joined || !errors.Is(observed[0], context.Canceled) || !errors.Is(observed[0], cause) {
		t.Fatalf("joined substantive cancellation was dropped or changed: %+v", observed)
	}
}

func TestNormalHandlerTerminationBoundsCustomErrorChains(t *testing.T) {
	cycle := &customSessionError{}
	cycle.cause = cycle
	if normalHandlerTermination(cycle) {
		t.Fatal("cyclic custom error chain was treated as normal termination")
	}

	var typedNil error = (*customSessionError)(nil)
	if normalHandlerTermination(typedNil) {
		t.Fatal("typed nil error was treated as normal termination")
	}

	deep := error(io.EOF)
	for range 17 {
		deep = &customSessionError{cause: deep}
	}
	if normalHandlerTermination(deep) {
		t.Fatal("error chain beyond the traversal bound was treated as normal termination")
	}

	// A non-comparable custom multi-error cannot be entered in the cycle set;
	// the node bound must still make this cyclic chain terminate conservatively.
	multi := customSessionErrors{nil}
	multi[0] = multi
	if normalHandlerTermination(multi) {
		t.Fatal("cyclic non-comparable multi-error was treated as normal termination")
	}
}

type customSessionError struct{ cause error }

func (*customSessionError) Error() string   { return "custom session error" }
func (e *customSessionError) Unwrap() error { return e.cause }

type customSessionErrors []error

func (customSessionErrors) Error() string     { return "custom session errors" }
func (e customSessionErrors) Unwrap() []error { return []error(e) }

func serveOneForTest(t *testing.T, service *Service, consumer string) {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	service.serveOne(context.Background(), streamauth.Header{Consumer: consumer}, server)
}
