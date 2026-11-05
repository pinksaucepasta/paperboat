package runtime

import (
	"context"
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/node"
)

// An edge restart against a control plane that accepts the new process's registration after a
// delay: a TLS handshake asks for an on-demand certificate first. The server refuses a
// distribution request until the node row carries this process epoch, so the request must wait
// for registration and the restart must log no control failure.
func TestOnDemandCertificateWaitsForNodeRegistration(t *testing.T) {
	const nodeID, epoch = "edge_01", "epoch_0001"
	var mu sync.Mutex
	var registered bool
	var order []string
	release := make(chan struct{})
	time.AfterFunc(200*time.Millisecond, func() { close(release) })
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/nodes/register":
			<-release
			mu.Lock()
			registered, order = true, append(order, "register")
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case "/v1/nodes/heartbeat":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "{}")
		case control.CertificateDistributionRequestPath:
			mu.Lock()
			defer mu.Unlock()
			order = append(order, "certificate_request")
			if !registered { // server/internal/tunnelcert/distribution_identity_sql.go: epoch must match the node row
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	var failures atomic.Int32
	client, err := control.NewHTTPClient(control.HTTPConfig{BaseURL: server.URL, Credential: strings.Repeat("c", 32), Timeout: 5 * time.Second, Client: server.Client(), ControlFailure: func(context.Context, string, error) { failures.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := control.NewEd25519DistributionRequestSigner(control.DistributionProofSignerConfig{PrivateKey: key, NodeID: nodeID, ProcessEpoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetDistributionRequestSigner(signer); err != nil {
		t.Fatal(err)
	}
	distribution, err := control.NewCertificateDistributionClient(client)
	if err != nil {
		t.Fatal(err)
	}
	registeredSignal := make(chan struct{})
	requester, err := control.NewOnDemandCertificateRequester(distribution, nodeID, epoch, registeredSignal)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := node.NewManager(node.New(nodeID), 1)
	if err != nil {
		t.Fatal(err)
	}
	worker := &NodeWorker{Manager: manager, Sink: client, Registration: testNodeRegistration(t, nodeID, epoch, 1), Interval: time.Hour, Registered: registeredSignal}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	requested := make(chan error, 1)
	go func() { requested <- requester.RequestCertificate(ctx, "app.example.test") }()
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer worker.Shutdown(context.Background())
	if err := <-requested; err != nil {
		t.Fatalf("certificate request: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(order, []string{"register", "certificate_request"}) || failures.Load() != 0 {
		t.Fatalf("order = %v, control failures = %d", order, failures.Load())
	}
}
