package tunnelmanager

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

// Hostd must agree with the edge and server on every ingress decision the shared contract can
// produce: the same fixtures (shared/testdata, the one copy) drive the same read and authorize
// calls admitIngress makes. A binding the edge admits and hostd denies is an outage.
func TestHostdAgreesWithTheSharedIngressContract(t *testing.T) {
	file, err := os.Open("../../../../shared/testdata/contracts/connector-v1/fixtures/ingress.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<16), 1<<20)
	seen := map[string]bool{}
	for scanner.Scan() {
		var vector struct {
			Case     string          `json:"case"`
			Valid    bool            `json:"valid"`
			Now      time.Time       `json:"now"`
			Decision json.RawMessage `json:"decision"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &vector); err != nil {
			t.Fatal(err)
		}
		var wire bytes.Buffer
		binary.Write(&wire, binary.BigEndian, uint32(len(vector.Decision)))
		wire.Write(vector.Decision)
		decision, err := connectorprotocol.ReadIngressDecision(&wire, vector.Now)
		if (err == nil) != vector.Valid {
			t.Errorf("%s: hostd read err=%v, valid=%v", vector.Case, err, vector.Valid)
			continue
		}
		if !vector.Valid {
			continue
		}
		b := decision.Binding
		kind := "http"
		if b.Protocol != "http" {
			kind = "tcp_public"
		}
		open := connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion,
			AccountID: b.AccountID, TunnelID: b.TunnelID, ConnectorID: decision.ConnectorID, SessionID: decision.SessionID,
			ProcessGeneration: decision.ProcessGeneration, Generation: decision.ConfigGeneration, RouteID: b.RouteID, RequestID: "request-1", Kind: kind}
		if err := decision.Authorize(decision, open, decision.EdgeNodeID, decision.EdgeProcessEpoch, vector.Now); err != nil {
			t.Errorf("%s: hostd authorize: %v", vector.Case, err)
		}
		seen[b.Protocol+"/"+b.Lifecycle+"/"+b.Audience] = true
		if decision.NativeAuthorization != nil {
			seen["native/"+b.Lifecycle] = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	// The shapes that once split hostd from the edge must stay in the fixtures.
	for _, want := range []string{"tls/durable/public", "tcp/durable/public", "native/durable", "native/ephemeral"} {
		if !seen[want] {
			t.Errorf("the shared ingress fixtures have no accepted %s vector", want)
		}
	}
}
