package connector

import (
	"errors"
)

type Transport string

const (
	Auto         Transport = "auto"
	QUIC         Transport = "quic"
	TCPDedicated Transport = "tcp_dedicated"
	TCPMux       Transport = "tcp_mux"
	HTTP3        Transport = "http3"
	HTTP2        Transport = "http2"
	TCPTLS       Transport = TCPMux
)

var (
	ErrAdmissionInvalid  = errors.New("connector admission invalid")
	ErrAdmissionReplayed = errors.New("connector admission replayed")
	ErrGenerationStale   = errors.New("connector generation stale")
	ErrUnavailable       = errors.New("connector transport unavailable")
	ErrShuttingDown      = errors.New("connector shutting down")
)
