package connectorprotocol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"testing"
	"time"

	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

func TestControlTransportPersistsReadyHeartbeatAndGenerationSafeDisconnect(t *testing.T) {
	now := time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC)
	auth, private := testAuth(t, now, 1)
	snapshot, err := wire.NewSnapshot(auth.TunnelID, 1, testConfigPayload(1, "app.example.test"))
	if err != nil {
		t.Fatal(err)
	}
	store := &persistenceStoreStub{authResult: wire.AuthResult{
		AccountID: auth.AccountID, TunnelID: auth.TunnelID, ConnectorID: auth.ConnectorID,
		HostID: auth.HostID, IdentityKeyID: auth.IdentityKeyID, IdentityKeyThumbprint: auth.IdentityKeyThumbprint,
		ProcessGeneration: 1, CredentialGeneration: auth.CredentialGeneration, CredentialExpiresAt: now.Add(time.Hour), LeaseExpiresAt: now.Add(time.Hour),
	}, snapshot: snapshot}
	server, err := NewPersistentServer(store, IdentityProofVerifierFuncs{
		AuthFunc: func(_ context.Context, _ wire.AuthRequest, payload, signature []byte) error {
			if !ed25519.Verify(private.Public().(ed25519.PublicKey), payload, signature) {
				return wire.ErrAuthenticationFailed
			}
			return nil
		},
		RenewalFunc: func(context.Context, wire.RenewalRequest, []byte, []byte) error { return nil },
	}, wire.ServerConfig{Capabilities: wire.ProductionCapabilities(), Clock: &testClock{now: now}, LeaseDuration: time.Minute, HeartbeatInterval: 10 * time.Second, SessionIDs: func() (string, error) { return "session_transport_01", nil }})
	if err != nil {
		t.Fatal(err)
	}
	sessions := NewActiveControlSessions()
	rotationStore := &dispatcherStore{MemoryRotationPersistence: &wire.MemoryRotationPersistence{}}
	dispatcher, err := NewRotationDispatcher(RotationDispatcherConfig{Store: rotationStore, VerifyOldProof: rotationVerifier(nil), Clock: &testClock{now: now}, ReportError: func(error) {}})
	if err != nil {
		t.Fatal(err)
	}
	drainDispatcher, err := NewDrainDispatcher(DrainDispatcherConfig{Store: &drainOperationSourceStub{}, Clock: &testClock{now: now}, ReportError: func(error) {}})
	if err != nil {
		t.Fatal(err)
	}
	transport, err := NewControlTransportWithDispatchers(server, sessions, dispatcher, drainDispatcher)
	if err != nil {
		t.Fatal(err)
	}
	serverSide, clientSide := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- transport.Serve(context.Background(), serverSide, auth.TunnelID, auth.ConnectorID) }()

	hello := wire.Hello{Protocol: wire.ProtocolName, MinVersion: wire.ProtocolVersion, MaxVersion: wire.ProtocolVersion, AccountID: auth.AccountID, TunnelID: auth.TunnelID, ConnectorID: auth.ConnectorID, HostID: auth.HostID, ProcessGeneration: 1, Capabilities: wire.ProductionCapabilities(), Auth: auth}
	helloFrame, err := wire.NewFrame(wire.MessageHello, "hello_transport_01", hello)
	if err != nil || wire.WriteFrame(clientSide, helloFrame) != nil {
		t.Fatalf("write hello: %v", err)
	}
	welcomeFrame, err := wire.ReadFrame(clientSide)
	if err != nil || welcomeFrame.Type != wire.MessageWelcome {
		select {
		case serveErr := <-done:
			t.Fatalf("welcome frame=%+v err=%v serve_err=%v", welcomeFrame, err, serveErr)
		default:
			t.Fatalf("welcome frame=%+v err=%v", welcomeFrame, err)
		}
	}
	var welcome wire.Welcome
	if err = welcomeFrame.DecodePayload(&welcome); err != nil {
		t.Fatal(err)
	}
	snapshotFrame, err := wire.ReadFrame(clientSide)
	if err != nil || snapshotFrame.Type != wire.MessageSnapshot {
		t.Fatalf("snapshot frame=%+v err=%v", snapshotFrame, err)
	}
	var bound wire.Snapshot
	if err = snapshotFrame.DecodePayload(&bound); err != nil {
		t.Fatal(err)
	}
	var live RotationLiveSession
	var registered bool
	registrationDeadline := time.Now().Add(time.Second)
	for !registered && time.Now().Before(registrationDeadline) {
		live, registered = dispatcher.session(auth.ConnectorID)
		if !registered {
			time.Sleep(time.Millisecond)
		}
	}
	if !registered || live.Reference.SessionID != welcome.SessionID || live.CredentialGeneration != auth.CredentialGeneration {
		t.Fatalf("rotation live session = %+v registered=%t", live, registered)
	}
	drainOperation := DrainOperation{
		OperationID: "operation_transport_drain_01", OperationType: "connector.drain",
		AccountID: auth.AccountID, TunnelID: auth.TunnelID, ConnectorID: auth.ConnectorID, HostID: auth.HostID,
		SessionID: welcome.SessionID, ProcessGeneration: 1, ConfigGeneration: bound.Generation, ConfigContentHash: bound.ContentHash,
	}
	drainLive, drainRegistered := drainDispatcher.session(drainOperation)
	if !drainRegistered || drainLive.Session.Reference() != live.Reference {
		t.Fatalf("drain live session = %+v registered=%t", drainLive.Projection, drainRegistered)
	}
	drain := wire.Drain{AccountID: auth.AccountID, TunnelID: auth.TunnelID, ConnectorID: auth.ConnectorID, SessionID: welcome.SessionID, ProcessGeneration: 1, DrainID: "drain_transport_01", Generation: bound.Generation, ContentHash: bound.ContentHash, Deadline: now.Add(20 * time.Second), StopNewStreams: true, ForceAfterDeadline: true}
	drainFrame, err := wire.NewFrame(wire.MessageDrain, "drain_transport_01", drain)
	if err != nil {
		t.Fatal(err)
	}
	sendDone := make(chan error, 1)
	go func() { sendDone <- live.Send(context.Background(), drainFrame) }()
	receivedDrain, err := wire.ReadFrame(clientSide)
	if err != nil || receivedDrain.Type != wire.MessageDrain {
		t.Fatalf("outbound drain=%+v err=%v", receivedDrain, err)
	}
	if err = <-sendDone; err != nil {
		t.Fatal(err)
	}
	active, ok := sessions.Lookup(auth.TunnelID, auth.ConnectorID, welcome.SessionID, 1)
	if !ok || active.HostID != auth.HostID || active.ConfigGeneration != 1 || active.ConfigContentHash != bound.ContentHash {
		t.Fatalf("active session = %+v ok=%t", active, ok)
	}
	ack := wire.Ack{AccountID: auth.AccountID, TunnelID: auth.TunnelID, ConnectorID: auth.ConnectorID, SessionID: welcome.SessionID, ProcessGeneration: 1, Kind: wire.AckSnapshot, Status: wire.AckApplied, Generation: bound.Generation, ContentHash: bound.ContentHash}
	frame, _ := wire.NewFrame(wire.MessageAck, snapshotFrame.RequestID, ack)
	if err = wire.WriteFrame(clientSide, frame); err != nil {
		t.Fatal(err)
	}
	ready := wire.Readiness{AccountID: auth.AccountID, TunnelID: auth.TunnelID, ConnectorID: auth.ConnectorID, SessionID: welcome.SessionID, ProcessGeneration: 1, Generation: bound.Generation, ContentHash: bound.ContentHash, EdgeReady: true, RouteReady: true, OriginReady: true}
	frame, _ = wire.NewFrame(wire.MessageReady, "ready_transport_01", ready)
	if err = wire.WriteFrame(clientSide, frame); err != nil {
		t.Fatal(err)
	}
	readyAck, err := wire.ReadFrame(clientSide)
	if err != nil || readyAck.Type != wire.MessageAck {
		t.Fatalf("ready ack=%+v err=%v", readyAck, err)
	}
	serving := bound
	for _, generation := range []uint64{2, 3, 4} {
		payload := testConfigPayload(generation, "app.example.test")
		if generation == 2 {
			payload = bytes.Replace(payload, []byte(`"desired_state":"active"`), []byte(`"desired_state":"paused"`), 1)
		}
		desired, err := wire.NewSnapshot(auth.TunnelID, generation, payload)
		if err != nil {
			t.Fatal(err)
		}
		store.snapshotMu.Lock()
		store.snapshot = desired
		store.snapshotMu.Unlock()
		heartbeat := wire.Heartbeat{AccountID: auth.AccountID, TunnelID: auth.TunnelID, ConnectorID: auth.ConnectorID, SessionID: welcome.SessionID, ProcessGeneration: 1, LastAppliedGeneration: serving.Generation, LastAppliedHash: serving.ContentHash, SentAt: now.Add(time.Duration(generation) * time.Second)}
		frame, _ = wire.NewFrame(wire.MessageHeartbeat, "heartbeat_transport_01", heartbeat)
		if err = wire.WriteFrame(clientSide, frame); err != nil {
			t.Fatal(err)
		}
		heartbeatAck, err := wire.ReadFrame(clientSide)
		if err != nil || heartbeatAck.Type != wire.MessageHeartbeatAck {
			t.Fatalf("heartbeat ack=%+v err=%v", heartbeatAck, err)
		}
		_ = clientSide.SetReadDeadline(time.Now().Add(time.Second))
		update, err := wire.ReadFrame(clientSide)
		if err != nil || update.Type != wire.MessageSnapshot {
			t.Fatalf("live desired update=%+v err=%v", update, err)
		}
		_ = clientSide.SetReadDeadline(time.Time{})
		var next wire.Snapshot
		if err = update.DecodePayload(&next); err != nil || next.Generation != generation || next.SessionID != welcome.SessionID {
			t.Fatalf("new snapshot=%+v error=%v", next, err)
		}
		prior, ok := sessions.Lookup(auth.TunnelID, auth.ConnectorID, welcome.SessionID, 1)
		if !ok || prior.ConfigGeneration != bound.Generation {
			t.Fatal("unapplied desired config advanced bootstrap authority")
		}
		ack.Generation, ack.ContentHash = next.Generation, next.ContentHash
		frame, _ = wire.NewFrame(wire.MessageAck, update.RequestID, ack)
		if err = wire.WriteFrame(clientSide, frame); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(time.Second)
		for {
			projected, exists := sessions.Lookup(auth.TunnelID, auth.ConnectorID, welcome.SessionID, 1)
			if exists && projected.ConfigGeneration == next.Generation {
				if projected.ServingConfigGeneration != serving.Generation || projected.ServingConfigContentHash != serving.ContentHash {
					t.Fatal("candidate ACK revoked serving bootstrap tuple")
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("candidate ACK not reflected")
			}
			time.Sleep(time.Millisecond)
		}
		if generation == 3 {
			bound = next
			continue
		} // Applied but failed readiness; next desired snapshot must recover.
		ready.Generation, ready.ContentHash = next.Generation, next.ContentHash
		frame, _ = wire.NewFrame(wire.MessageReady, "ready_transport_01", ready)
		if err = wire.WriteFrame(clientSide, frame); err != nil {
			t.Fatal(err)
		}
		readyAck, err = wire.ReadFrame(clientSide)
		if err != nil || readyAck.Type != wire.MessageAck {
			t.Fatalf("new ready ack=%+v err=%v", readyAck, err)
		}
		active, ok = sessions.Lookup(auth.TunnelID, auth.ConnectorID, welcome.SessionID, 1)
		if !ok || active.ConfigGeneration != generation || active.ConfigContentHash != next.ContentHash || active.ServingConfigGeneration != next.Generation || active.ServingConfigContentHash != next.ContentHash {
			t.Fatal("applied config did not advance bootstrap authority")
		}
		bound = next
		serving = next
	}
	// A heartbeat on an unchanged generation reads no snapshot payload and gets no snapshot (F21-22).
	store.snapshotMu.Lock()
	readsBefore := store.snapshotReads
	store.snapshotMu.Unlock()
	steady := wire.Heartbeat{AccountID: auth.AccountID, TunnelID: auth.TunnelID, ConnectorID: auth.ConnectorID, SessionID: welcome.SessionID, ProcessGeneration: 1, LastAppliedGeneration: serving.Generation, LastAppliedHash: serving.ContentHash, SentAt: now.Add(10 * time.Second)}
	frame, _ = wire.NewFrame(wire.MessageHeartbeat, "heartbeat_transport_steady", steady)
	if err = wire.WriteFrame(clientSide, frame); err != nil {
		t.Fatal(err)
	}
	if steadyAck, err := wire.ReadFrame(clientSide); err != nil || steadyAck.Type != wire.MessageHeartbeatAck {
		t.Fatalf("steady heartbeat ack=%+v err=%v", steadyAck, err)
	}
	_ = clientSide.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if extra, err := wire.ReadFrame(clientSide); err == nil {
		t.Fatalf("an unchanged generation sent %s", extra.Type)
	}
	_ = clientSide.SetReadDeadline(time.Time{})
	store.snapshotMu.Lock()
	readsAfter := store.snapshotReads
	store.snapshotMu.Unlock()
	if readsAfter != readsBefore {
		t.Fatalf("a heartbeat on an unchanged generation read the snapshot payload %d times", readsAfter-readsBefore)
	}
	disconnect := wire.Disconnect{AccountID: auth.AccountID, TunnelID: auth.TunnelID, ConnectorID: auth.ConnectorID, SessionID: welcome.SessionID, ProcessGeneration: 1, Reason: wire.ReasonProtocolClosed}
	frame, _ = wire.NewFrame(wire.MessageDisconnect, "disconnect_transport_01", disconnect)
	if err = wire.WriteFrame(clientSide, frame); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if _, ok = sessions.Lookup(auth.TunnelID, auth.ConnectorID, welcome.SessionID, 1); ok || len(store.applied) != 4 || len(store.ready) != 3 || len(store.heartbeats) != 4 || len(store.disconnect) != 1 {
		t.Fatalf("active=%t applied=%d ready=%d heartbeats=%d disconnects=%d", ok, len(store.applied), len(store.ready), len(store.heartbeats), len(store.disconnect))
	}
	if _, ok = dispatcher.session(auth.ConnectorID); ok {
		t.Fatal("rotation delivery route remained after control disconnect")
	}
	if _, ok = drainDispatcher.session(drainOperation); ok {
		t.Fatal("drain delivery route remained after control disconnect")
	}
}

func TestControlTransportRejectsPathIdentityBeforeSessionCreation(t *testing.T) {
	now := time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC)
	auth, _ := testAuth(t, now, 1)
	snapshot, _ := wire.NewSnapshot(auth.TunnelID, 1, testConfigPayload(1, "app.example.test"))
	store := &persistenceStoreStub{snapshot: snapshot}
	server, err := NewPersistentServer(store, IdentityProofVerifierFuncs{
		AuthFunc: func(context.Context, wire.AuthRequest, []byte, []byte) error { return nil }, RenewalFunc: func(context.Context, wire.RenewalRequest, []byte, []byte) error { return nil },
	}, wire.ServerConfig{Capabilities: requiredCapabilityList(), Clock: &testClock{now: now}})
	if err != nil {
		t.Fatal(err)
	}
	transport, _ := NewControlTransport(server, nil)
	serverSide, clientSide := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- transport.Serve(context.Background(), serverSide, "wrong_tunnel", auth.ConnectorID) }()
	hello := wire.Hello{Protocol: wire.ProtocolName, MinVersion: wire.ProtocolVersion, MaxVersion: wire.ProtocolVersion, AccountID: auth.AccountID, TunnelID: auth.TunnelID, ConnectorID: auth.ConnectorID, HostID: auth.HostID, ProcessGeneration: 1, Capabilities: requiredCapabilityList(), Auth: auth}
	frame, _ := wire.NewFrame(wire.MessageHello, "hello_wrong_path_01", hello)
	if err = wire.WriteFrame(clientSide, frame); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, wire.ErrIdentityMismatch) || len(store.created) != 0 {
		t.Fatalf("err=%v created=%d", err, len(store.created))
	}
}

func TestActiveControlSessionsOldDetachCannotRemoveReplacement(t *testing.T) {
	registry := NewActiveControlSessions()
	base := ActiveControlSession{AccountID: "account_01", TunnelID: "tunnel_01", ConnectorID: "connector_01", HostID: "host_01", SessionID: "session_old_01", IdentityKeyID: "ed25519:" + testThumbprint, IdentityKeyThumbprint: testThumbprint, ProcessGeneration: 1, CredentialGeneration: 1, ConfigGeneration: 1, ConfigContentHash: "sha256:" + string(make([]byte, 64))}
	// Use a syntactically valid hash; content is not interpreted by the registry.
	base.ConfigContentHash = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	oldSession := &PersistentSession{}
	if err := registry.attach(base, oldSession); err != nil {
		t.Fatal(err)
	}
	replacement := base
	replacement.SessionID, replacement.ProcessGeneration = "session_new_01", 2
	if err := registry.attach(replacement, &PersistentSession{}); err != nil {
		t.Fatal(err)
	}
	registry.detach(base)
	if got, ok := registry.Lookup(replacement.TunnelID, replacement.ConnectorID, replacement.SessionID, replacement.ProcessGeneration); !ok || got.SessionID != replacement.SessionID {
		t.Fatalf("replacement removed: %+v ok=%t", got, ok)
	}
}

const testThumbprint = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestControlTransportRejectsStaleSnapshotAckBeforePersistence(t *testing.T) {
	session, _, _, _, first := testSessionPair(t)
	next, err := wire.NewSnapshot("tunnel_1", 2, testConfigPayload(2, "next.example.test"))
	if err != nil {
		t.Fatal(err)
	}
	if err = session.OfferSnapshot(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	store := &persistenceStoreStub{}
	persistent := &PersistentSession{session: session, store: store}
	stale := wire.Ack{AccountID: "acct_1", TunnelID: "tunnel_1", ConnectorID: "connector_1", SessionID: "sess_1", ProcessGeneration: 1, Kind: wire.AckSnapshot, Status: wire.AckApplied, Generation: first.Generation, ContentHash: first.ContentHash}
	frame, err := wire.NewFrame(wire.MessageAck, "stale_ack", stale)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = handleControlFrame(context.Background(), persistent, nil, frame); !errors.Is(err, wire.ErrSnapshotRejected) {
		t.Fatalf("stale ACK accepted: %v", err)
	}
	current, ready, _ := session.Current()
	if !ready || current.Generation != first.Generation || len(store.applied) != 0 {
		t.Fatal("stale ACK altered applied/readiness authority")
	}
}
