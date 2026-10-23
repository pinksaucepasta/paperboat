package connector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// TransportDialError lets a dialer make fallback eligibility explicit.  A
// plain net.Error is also eligible, except context cancellation/deadlines,
// which must never silently switch transports.
type TransportDialError struct {
	Transport Transport
	Err       error
	Fallback  bool
}

func (e *TransportDialError) Error() string {
	return "connector transport failed"
}

func (e *TransportDialError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (*TransportDialError) DiagnosticStage() string { return "peer_connect" }

func (*TransportDialError) DiagnosticCode() string { return "transport_failed" }

func transportFallbackAllowed(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var dialErr *TransportDialError
	if errors.As(err, &dialErr) {
		return dialErr.Fallback
	}
	var networkErr net.Error
	return errors.As(err, &networkErr)
}

type DataCarrierTarget struct {
	EdgeID        string
	ProcessEpoch  string
	FailureDomain string
}

type DataCarrierPoolConfig struct {
	Targets         []DataCarrierTarget
	RefreshTargets  func(context.Context) ([]DataCarrierTarget, time.Time, error)
	TargetsExpireAt time.Time
	MaximumCarriers int
	QueueDepth      int
	Preferred       Transport
	Fallback        Transport
	SingleTransport bool
	EdgeID          string
	// EdgeProcessEpoch is the edge process this pool's carriers are admitted to. Accepted streams
	// carry it in their EdgeTarget, which ingress authority must match before an origin is reached.
	EdgeProcessEpoch string
	FailureDomains   []string
	Session          DataCarrierIdentity
	Carrier          DataCarrierConfig
}

func DefaultDataCarrierPoolConfig() DataCarrierPoolConfig {
	return DataCarrierPoolConfig{
		MaximumCarriers: defaultDataCarrierMaxCarriers,
		QueueDepth:      defaultDataCarrierQueue,
		Preferred:       QUIC,
		Fallback:        TCPMux,
		EdgeID:          "edge",
		FailureDomains:  []string{"domain-a", "domain-b"},
		Session:         DataCarrierIdentity{AccountID: "account", HostID: "host-a", TunnelID: "tunnel", ConnectorID: "connector", SessionID: "session", ProcessGeneration: 1, Generation: 1},
		Carrier:         DefaultDataCarrierConfig(),
	}
}

func (c DataCarrierPoolConfig) withDefaults() DataCarrierPoolConfig {
	d := DefaultDataCarrierPoolConfig()
	if c.MaximumCarriers == 0 {
		c.MaximumCarriers = d.MaximumCarriers
	}
	if c.QueueDepth == 0 {
		c.QueueDepth = d.QueueDepth
	}
	if c.Preferred == "" || c.Preferred == Auto {
		c.Preferred = d.Preferred
	}
	if c.Fallback == "" || c.Fallback == Auto {
		c.Fallback = d.Fallback
	}
	if c.EdgeID == "" {
		c.EdgeID = d.EdgeID
	}
	if len(c.FailureDomains) == 0 {
		c.FailureDomains = append([]string(nil), d.FailureDomains...)
	}
	c.Carrier = c.Carrier.withDefaults()
	return c
}

func (c DataCarrierPoolConfig) Validate() error {
	c = c.withDefaults()
	if c.MaximumCarriers <= 0 || c.MaximumCarriers > 16 {
		return fmt.Errorf("%w: maximum carriers must be in [1,16]", ErrInvalidDataCarrierConfig)
	}
	if c.QueueDepth <= 0 || c.QueueDepth > maxDataCarrierQueue {
		return fmt.Errorf("%w: pool queue depth must be in [1,%d]", ErrInvalidDataCarrierConfig, maxDataCarrierQueue)
	}
	if c.Preferred != QUIC && c.Preferred != TCPMux && c.Preferred != HTTP3 && c.Preferred != HTTP2 {
		return fmt.Errorf("%w: unsupported preferred transport", ErrInvalidDataCarrierConfig)
	}
	if c.Fallback != QUIC && c.Fallback != TCPMux && c.Fallback != HTTP3 && c.Fallback != HTTP2 {
		return fmt.Errorf("%w: unsupported fallback transport", ErrInvalidDataCarrierConfig)
	}
	if c.Preferred == c.Fallback && !c.SingleTransport {
		return fmt.Errorf("%w: equal preferred and fallback require single transport", ErrInvalidDataCarrierConfig)
	}
	if c.SingleTransport && c.Preferred != c.Fallback {
		return fmt.Errorf("%w: single transport requires equal preferred and fallback", ErrInvalidDataCarrierConfig)
	}
	if !validDataCarrierIdentifier(c.EdgeID, 128) {
		return fmt.Errorf("%w: edge identity is required", ErrInvalidDataCarrierConfig)
	}
	if len(c.Targets) == 0 && len(c.FailureDomains) < c.MaximumCarriers {
		return fmt.Errorf("%w: one failure domain is required per carrier slot", ErrInvalidDataCarrierConfig)
	}
	if len(c.Targets) != 0 {
		if len(c.Targets) != c.MaximumCarriers || len(c.Targets) > 2 {
			return ErrInvalidDataCarrierConfig
		}
		seen := map[string]bool{}
		for _, target := range c.Targets {
			if !validDataCarrierIdentifier(target.EdgeID, 128) || !validDataCarrierIdentifier(target.ProcessEpoch, 128) || !validDataCarrierIdentifier(target.FailureDomain, 128) || seen[target.EdgeID] {
				return ErrInvalidDataCarrierConfig
			}
			seen[target.EdgeID] = true
		}
	}
	if err := c.Session.validate(); err != nil {
		return fmt.Errorf("%w: authenticated session identity is required", ErrInvalidDataCarrierConfig)
	}
	seenDomains := make(map[string]struct{}, len(c.FailureDomains))
	for _, domain := range c.FailureDomains {
		if !validDataCarrierIdentifier(domain, 128) {
			return fmt.Errorf("%w: failure domain is required", ErrInvalidDataCarrierConfig)
		}
		if _, exists := seenDomains[domain]; exists && len(c.Targets) == 0 {
			return fmt.Errorf("%w: failure domains must be distinct", ErrInvalidDataCarrierConfig)
		}
		seenDomains[domain] = struct{}{}
	}
	if err := c.Carrier.Validate(); err != nil {
		return err
	}
	return nil
}

func validDataCarrierIdentifier(value string, maximum int) bool {
	if len(value) == 0 || len(value) > maximum {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

// DataCarrierDialRequest binds each dial attempt to a configured edge and
// failure domain.  A dialer must not silently reuse a different slot or
// domain, because the pool uses those identities for failover diagnostics.
type DataCarrierDialRequest struct {
	ProcessEpoch  string
	Transport     Transport
	Slot          int
	Attempt       int
	EdgeID        string
	FailureDomain string
	Identity      DataCarrierIdentity
}

// DataCarrierDialResult records the identity actually reached by a dialer.
// The pool rejects a result which does not match its request. Production
// dialers must populate PeerIdentity from the verified TLS/QUIC peer binding.
type DataCarrierDialResult struct {
	Link          io.ReadWriteCloser
	Session       DataCarrierSession
	PeerIdentity  DataCarrierIdentity
	Transport     Transport
	EdgeID        string
	FailureDomain string
}

type DataCarrierDialer func(context.Context, DataCarrierDialRequest) (DataCarrierDialResult, error)

type DataCarrierPoolState string

const (
	DataCarrierPoolDisconnected DataCarrierPoolState = "disconnected"
	DataCarrierPoolConnecting   DataCarrierPoolState = "connecting"
	DataCarrierPoolReady        DataCarrierPoolState = "ready"
	DataCarrierPoolDraining     DataCarrierPoolState = "draining"
	DataCarrierPoolClosed       DataCarrierPoolState = "closed"
)

type DataCarrierInfo struct {
	ProcessEpoch  string
	ID            string
	Identity      DataCarrierIdentity
	Transport     Transport
	EdgeID        string
	FailureDomain string
	Slot          int
	Attempt       int
	ActiveStreams int
	State         DataCarrierState
}

type pooledDataCarrier struct {
	target        DataCarrierTarget
	transport     Transport
	edgeID        string
	failureDomain string
	slot          int
	attempt       int
	carrier       *DataCarrier
}

// DataCarrierPool maintains a small set of independently dialed carriers.
// Connect publishes Ready only after at least one preferred/fallback carrier
// has completed a yamux Ping round trip.
type carrierSlotRetry struct {
	failures int
	next     time.Time
}
