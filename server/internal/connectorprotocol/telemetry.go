package connectorprotocol

import (
	"time"

	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"

	servertelemetry "github.com/pinksaucepasta/paperboat/server/internal/telemetry"
)

// ConnectorTelemetry is the connector-v1 adapter for the server-owned
// telemetry producer. It accepts only protocol identities and finite states;
// it never forwards tokens, public keys, host names, URLs, or error text.
//
// The adapter deliberately treats telemetry as best effort. Every method
// drops producer errors so an exhausted event queue or a telemetry validation
// problem cannot change a successful protocol or database transition.
type ConnectorTelemetry struct {
	Producer *servertelemetry.Producer
}

func NewConnectorTelemetry(producer *servertelemetry.Producer) *ConnectorTelemetry {
	if producer == nil {
		return nil
	}
	return &ConnectorTelemetry{Producer: producer}
}

type connectorTelemetryIdentity struct {
	AccountID            string
	Reference            wire.SessionRef
	CredentialGeneration uint64
	ConfigGeneration     uint64
}

func (t *ConnectorTelemetry) identity(value connectorTelemetryIdentity) (servertelemetry.ProducerIdentity, servertelemetry.Generations) {
	if wire.ValidateIdentifier(value.Reference.TunnelID) != nil || wire.ValidateIdentifier(value.Reference.ConnectorID) != nil || value.Reference.ProcessGeneration == 0 {
		return servertelemetry.ProducerIdentity{}, servertelemetry.Generations{}
	}
	identity := servertelemetry.ProducerIdentity{
		// A session is the connector resource for this stream. Keeping the
		// resource identity stable lets the lifecycle stream correlate all
		// transitions without exposing the connector's user-facing name.
		ResourceKind: "connector",
		ResourceID:   value.Reference.ConnectorID,
		IDs: servertelemetry.SafeIDs{
			AccountID:   value.AccountID,
			TunnelID:    value.Reference.TunnelID,
			ConnectorID: value.Reference.ConnectorID,
		},
	}
	if wire.ValidateIdentifier(value.Reference.SessionID) == nil {
		identity.IDs.SessionID = value.Reference.SessionID
	}
	if wire.ValidateIdentifier(value.Reference.SessionID) == nil && len(value.Reference.SessionID) <= 124 {
		identity.CorrelationID = "cor_" + value.Reference.SessionID
	}
	return identity, servertelemetry.Generations{
		Process:    value.Reference.ProcessGeneration,
		Credential: value.CredentialGeneration,
		Config:     value.ConfigGeneration,
	}
}

func (t *ConnectorTelemetry) identityWithHost(value connectorTelemetryIdentity, hostID string) (servertelemetry.ProducerIdentity, servertelemetry.Generations) {
	identity, generations := t.identity(value)
	identity.IDs.HostID = hostID
	return identity, generations
}

func usableConnectorTelemetryIdentity(identity servertelemetry.ProducerIdentity, generations servertelemetry.Generations) bool {
	// Failed handshakes can arrive before the protocol has accepted the
	// connector identity. Do not publish an event with generated/empty IDs for
	// those attempts; telemetry must describe an authenticated or safely bound
	// connector only.
	return identity.ResourceKind == "connector" && identity.ResourceID != "" && generations.Process > 0
}

func (t *ConnectorTelemetry) handshake(value connectorTelemetryIdentity, hostID string, outcome servertelemetry.ConnectorHandshakeOutcome, duration time.Duration) {
	if t == nil || t.Producer == nil {
		return
	}
	identity, generations := t.identityWithHost(value, hostID)
	if !usableConnectorTelemetryIdentity(identity, generations) {
		return
	}
	_ = t.Producer.RecordConnectorHandshake(servertelemetry.ConnectorHandshakeInput{
		Outcome: outcome, Duration: boundedTelemetryDuration(duration), Identity: identity, Generations: generations,
	})
}

func (t *ConnectorTelemetry) connection(value connectorTelemetryIdentity, hostID string, outcome servertelemetry.ConnectorConnectionOutcome) {
	if t == nil || t.Producer == nil {
		return
	}
	identity, generations := t.identityWithHost(value, hostID)
	if !usableConnectorTelemetryIdentity(identity, generations) {
		return
	}
	_ = t.Producer.RecordConnectorConnection(servertelemetry.ConnectorConnectionInput{
		Outcome: outcome, Identity: identity, Generations: generations,
	})
}

func (t *ConnectorTelemetry) session(value connectorTelemetryIdentity, hostID string, state servertelemetry.ConnectorSessionState, configGeneration uint64) {
	if t == nil || t.Producer == nil {
		return
	}
	value.ConfigGeneration = configGeneration
	identity, generations := t.identityWithHost(value, hostID)
	if !usableConnectorTelemetryIdentity(identity, generations) {
		return
	}
	_ = t.Producer.RecordConnectorSession(servertelemetry.ConnectorSessionInput{
		State: state, Identity: identity, Generations: generations,
	})
}

func (t *ConnectorTelemetry) reconnect(value connectorTelemetryIdentity, hostID string, outcome servertelemetry.ConnectorReconnectOutcome) {
	if t == nil || t.Producer == nil {
		return
	}
	identity, generations := t.identityWithHost(value, hostID)
	if !usableConnectorTelemetryIdentity(identity, generations) {
		return
	}
	_ = t.Producer.RecordConnectorReconnect(servertelemetry.ConnectorReconnectInput{
		Outcome: outcome, Identity: identity, Generations: generations,
	})
}

func (t *ConnectorTelemetry) backoff(value connectorTelemetryIdentity, hostID string, state servertelemetry.ConnectorBackoffState, duration time.Duration) {
	if t == nil || t.Producer == nil {
		return
	}
	identity, generations := t.identityWithHost(value, hostID)
	if !usableConnectorTelemetryIdentity(identity, generations) {
		return
	}
	_ = t.Producer.RecordConnectorBackoff(servertelemetry.ConnectorBackoffInput{
		State: state, Duration: boundedTelemetryDuration(duration), Identity: identity, Generations: generations,
	})
}

func (t *ConnectorTelemetry) disconnect(value connectorTelemetryIdentity, hostID string, reason wire.DisconnectReason) {
	if t == nil || t.Producer == nil {
		return
	}
	identity, generations := t.identityWithHost(value, hostID)
	if !usableConnectorTelemetryIdentity(identity, generations) {
		return
	}
	_ = t.Producer.RecordConnectorDisconnect(servertelemetry.ConnectorDisconnectInput{
		Reason: mapTelemetryDisconnectReason(reason), Identity: identity, Generations: generations,
	})
}

func (t *ConnectorTelemetry) config(value connectorTelemetryIdentity, hostID string, state servertelemetry.ConfigGenerationState, generation uint64, retry servertelemetry.RetryDecision, nextRetryAt time.Time) {
	if t == nil || t.Producer == nil {
		return
	}
	value.ConfigGeneration = generation
	identity, generations := t.identityWithHost(value, hostID)
	if !usableConnectorTelemetryIdentity(identity, generations) {
		return
	}
	_ = t.Producer.RecordConfigApply(servertelemetry.ConfigApplyInput{
		State: state, Generation: generation, Retry: retry, NextRetryAt: nextRetryAt,
		Identity: identity, Generations: generations,
	})
}

func boundedTelemetryDuration(value time.Duration) time.Duration {
	if value < 0 {
		return 0
	}
	if value > 24*time.Hour {
		return 24 * time.Hour
	}
	return value
}

func mapTelemetryDisconnectReason(reason wire.DisconnectReason) servertelemetry.ConnectorDisconnectReason {
	switch reason {
	case wire.ReasonAuthentication, wire.ReasonCredentialExpired:
		return servertelemetry.ConnectorDisconnectAuth
	case wire.ReasonLeaseExpired, wire.ReasonHeartbeatTimeout:
		return servertelemetry.ConnectorDisconnectNetwork
	case wire.ReasonServerShutdown:
		return servertelemetry.ConnectorDisconnectShutdown
	case wire.ReasonProtocolMismatch, wire.ReasonCapabilityMissing, wire.ReasonMalformed, wire.ReasonSnapshotRejected, wire.ReasonGenerationGap, wire.ReasonCredentialRotation, wire.ReasonCanceled, wire.ReasonProtocolClosed:
		return servertelemetry.ConnectorDisconnectProtocol
	case wire.ReasonSessionReplaced, wire.ReasonStaleGeneration:
		return servertelemetry.ConnectorDisconnectServer
	default:
		return servertelemetry.ConnectorDisconnectUnknown
	}
}

func (s *PersistentServer) SetTelemetryProducer(producer *servertelemetry.Producer) {
	if s == nil {
		return
	}
	s.telemetryMu.Lock()
	s.telemetry = NewConnectorTelemetry(producer)
	s.telemetryMu.Unlock()
}

func (s *PersistentServer) connectorTelemetry() *ConnectorTelemetry {
	if s == nil {
		return nil
	}
	s.telemetryMu.RLock()
	defer s.telemetryMu.RUnlock()
	return s.telemetry
}

func (s *PersistentSession) connectorTelemetry() *ConnectorTelemetry {
	if s == nil || s.parent == nil {
		return nil
	}
	return s.parent.connectorTelemetry()
}

func (s *PersistentSession) recordReadyTelemetry(readiness wire.Readiness) {
	if s == nil || s.session == nil {
		return
	}
	accountID, hostID, ref, credentialGeneration, _ := s.session.Metadata()
	observer := s.connectorTelemetry()
	if observer == nil {
		return
	}
	value := connectorTelemetryIdentity{AccountID: accountID, Reference: ref, CredentialGeneration: credentialGeneration, ConfigGeneration: readiness.Generation}
	observer.session(value, hostID, servertelemetry.ConnectorSessionActive, readiness.Generation)
}

func (s *PersistentSession) recordConfigTelemetry(ack wire.Ack, retry servertelemetry.RetryDecision, nextRetryAt time.Time) {
	if s == nil || s.session == nil {
		return
	}
	accountID, hostID, ref, credentialGeneration, _ := s.session.Metadata()
	observer := s.connectorTelemetry()
	if observer == nil {
		return
	}
	value := connectorTelemetryIdentity{AccountID: accountID, Reference: ref, CredentialGeneration: credentialGeneration, ConfigGeneration: ack.Generation}
	observer.config(value, hostID, servertelemetry.ConfigGenerationApplied, ack.Generation, retry, nextRetryAt)
}

func (s *PersistentSession) recordDesiredTelemetry(snapshot wire.Snapshot, retry servertelemetry.RetryDecision, nextRetryAt time.Time) {
	if s == nil || s.session == nil {
		return
	}
	accountID, hostID, ref, credentialGeneration, _ := s.session.Metadata()
	observer := s.connectorTelemetry()
	if observer == nil {
		return
	}
	value := connectorTelemetryIdentity{AccountID: accountID, Reference: ref, CredentialGeneration: credentialGeneration, ConfigGeneration: snapshot.Generation}
	observer.config(value, hostID, servertelemetry.ConfigGenerationDesired, snapshot.Generation, retry, nextRetryAt)
}

func (s *PersistentSession) currentSnapshotForTelemetry(generation uint64) wire.Snapshot {
	if s == nil || s.session == nil {
		return wire.Snapshot{Generation: generation}
	}
	_, _, ref, _, _ := s.session.Metadata()
	return wire.Snapshot{TunnelID: ref.TunnelID, Generation: generation}
}

func (s *PersistentSession) recordClosedTelemetry() {
	if s == nil || s.session == nil {
		return
	}
	if s.session.State() != wire.SessionClosed {
		return
	}
	s.telemetryCloseOnce.Do(func() {
		accountID, hostID, ref, credentialGeneration, configGeneration := s.session.Metadata()
		observer := s.connectorTelemetry()
		if observer == nil {
			return
		}
		value := connectorTelemetryIdentity{AccountID: accountID, Reference: ref, CredentialGeneration: credentialGeneration, ConfigGeneration: configGeneration}
		observer.session(value, hostID, servertelemetry.ConnectorSessionClosed, configGeneration)
		observer.connection(value, hostID, servertelemetry.ConnectorConnectionClosed)
		observer.disconnect(value, hostID, s.session.Disconnect().Reason)
	})
}
