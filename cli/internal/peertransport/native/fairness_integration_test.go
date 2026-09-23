package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sort"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"go.uber.org/goleak"
	"tailscale.com/tstest/integration"
)

const (
	fairnessTransferBytes = 32 << 20
	fairnessSamples       = 100
	fairnessP95           = 250 * time.Millisecond
	fairnessMaximum       = time.Second
)

// TestNativeBulkDoesNotStarveInteractiveAndControl measures the declared Task
// 20 workload on one QUIC connection. It deliberately uses ordinary bounded
// writes: scheduling belongs here only if this gate demonstrates a failure.
func TestNativeBulkDoesNotStarveInteractiveAndControl(t *testing.T) {
	leaks := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, leaks) })
	t.Setenv("IN_TS_TEST", "true")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	dm := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	signerPublic, signerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, clientFingerprint := testTLS(t, "fairness-cli")
	serverTLS, serverFingerprint := testTLS(t, "fairness-machine")
	clientBinding := tailnet.NetworkBinding{AccountID: "account_fairness", EndpointID: "cli_fairness", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::81"}
	serverBinding := tailnet.NetworkBinding{AccountID: "account_fairness", EndpointID: "machine_fairness", Role: "machine", MachineID: "machine_fairness", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::82"}
	clientAuthority := testAuthority(t, clientBinding, testKeys{"native_test": signerPublic})
	serverAuthority := testAuthority(t, serverBinding, testKeys{"native_test": signerPublic})
	clientBinding.WireGuardPublicKey, clientBinding.KeyGeneration = prepareBinding(t, clientAuthority)
	serverBinding.WireGuardPublicKey, serverBinding.KeyGeneration = prepareBinding(t, serverAuthority)
	now := time.Now().Unix()
	applyTestConfiguration(t, clientAuthority, signerPrivate, testConfiguration(now, 1, clientBinding, serverBinding, "dial"))
	applyTestConfiguration(t, serverAuthority, signerPrivate, testConfiguration(now, 1, serverBinding, clientBinding, "accept"))
	clientOwner, err := native.NewOwner(native.Config{Authority: clientAuthority, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer clientOwner.Close()
	serverOwner, err := native.NewOwner(native.Config{Authority: serverAuthority, TLS: serverTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer serverOwner.Close()

	descriptor, err := serverAuthority.Listen(dm.Regions[1])
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- serverOwner.Listen(ctx, dm.Regions[1], serveFairnessSession) }()
	defer func() {
		cancel()
		_ = serverOwner.Close()
		select {
		case <-serveDone:
		case <-time.After(2 * time.Second):
			t.Error("fairness listener did not stop")
		}
	}()
	session, err := clientOwner.Dial(ctx, descriptor.Address(), "machine_fairness", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}

	bulkStarted := make(chan struct{}, 2)
	bulkDone := make(chan error, 2)
	for transfer := 0; transfer < 2; transfer++ {
		go runFairnessBulk(ctx, session, transfer, bulkStarted, bulkDone)
	}
	for range 2 {
		select {
		case <-bulkStarted:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	terminal := openFairnessStream(t, ctx, session, "terminal", "terminal")
	defer terminal.Close()
	control := openFairnessStream(t, ctx, session, "exec", "terminal")
	defer control.Close()
	terminalLatency := sampleFairness(t, terminal, fairnessSamples)
	controlLatency := sampleFairness(t, control, fairnessSamples)
	select {
	case err := <-bulkDone:
		t.Fatalf("bulk traffic did not overlap the complete interactive workload: %v", err)
	default:
	}
	for range 2 {
		select {
		case err := <-bulkDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatalf("fairness workload exceeded its 30-second bound: %v", ctx.Err())
		}
	}
	assertFairness(t, "terminal", terminalLatency)
	assertFairness(t, "control", controlLatency)
}

func serveFairnessSession(ctx context.Context, session *native.Session) error {
	for {
		connection, header, err := session.AcceptAuthorized(ctx, func(context.Context, streamauth.Header) (string, error) { return "grant_test", nil })
		if err != nil {
			return err
		}
		go func() {
			defer connection.Close()
			if header.Consumer == "file_transfer" {
				hash := sha256.New()
				if _, err := io.CopyN(hash, connection, fairnessTransferBytes); err == nil {
					_, _ = connection.Write(hash.Sum(nil))
				}
				return
			}
			var value [8]byte
			for {
				if _, err := io.ReadFull(connection, value[:]); err != nil {
					return
				}
				if _, err := connection.Write(value[:]); err != nil {
					return
				}
			}
		}()
	}
}

func runFairnessBulk(ctx context.Context, session *native.Session, ordinal int, started chan<- struct{}, done chan<- error) {
	header, err := streamauth.New("fairness_bulk", "file_transfer", "fairness_bulk_"+string(rune('a'+ordinal)), "credential_file_transfer", time.Now().Add(time.Minute), fairnessTransferBytes+sha256.Size)
	if err != nil {
		done <- err
		return
	}
	connection, err := session.OpenAuthorized(ctx, header, "grant_test", "file_transfer")
	if err != nil {
		done <- err
		return
	}
	defer connection.Close()
	chunk := make([]byte, 1<<20)
	for i := range chunk {
		chunk[i] = byte(ordinal + 1)
	}
	full := sha256.New()
	started <- struct{}{}
	for written := 0; written < fairnessTransferBytes; written += len(chunk) {
		if _, err = connection.Write(chunk); err != nil {
			done <- err
			return
		}
		_, _ = full.Write(chunk)
	}
	var digest [sha256.Size]byte
	if _, err = io.ReadFull(connection, digest[:]); err == nil && !errors.Is(ctx.Err(), context.Canceled) {
		if string(digest[:]) != string(full.Sum(nil)) {
			err = errors.New("bulk digest mismatch")
		}
	}
	done <- err
}

func openFairnessStream(t *testing.T, ctx context.Context, session *native.Session, consumer, capability string) net.Conn {
	t.Helper()
	header, err := streamauth.New("fairness_"+consumer, consumer, "fairness_"+consumer, "credential_"+consumer, time.Now().Add(time.Minute), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := session.OpenAuthorized(ctx, header, "grant_test", capability)
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

func sampleFairness(t *testing.T, connection net.Conn, samples int) []time.Duration {
	t.Helper()
	result := make([]time.Duration, 0, samples)
	for sample := 0; sample < samples; sample++ {
		var request [8]byte
		binary.BigEndian.PutUint64(request[:], uint64(sample))
		started := time.Now()
		if _, err := connection.Write(request[:]); err != nil {
			t.Fatal(err)
		}
		var response [8]byte
		if _, err := io.ReadFull(connection, response[:]); err != nil {
			t.Fatal(err)
		}
		if response != request {
			t.Fatalf("response %d was lost or reordered", sample)
		}
		result = append(result, time.Since(started))
	}
	return result
}

func assertFairness(t *testing.T, name string, values []time.Duration) {
	t.Helper()
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	p95 := values[(len(values)*95+99)/100-1]
	maximum := values[len(values)-1]
	t.Logf("%s latency: p95=%s maximum=%s samples=%d", name, p95, maximum, len(values))
	if p95 > fairnessP95 || maximum > fairnessMaximum {
		t.Errorf("%s fairness missed gate: p95=%s (limit %s), maximum=%s (limit %s)", name, p95, fairnessP95, maximum, fairnessMaximum)
	}
}
