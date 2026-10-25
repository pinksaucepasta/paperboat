package connectorprotocol

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestGoawayNoticeRoundTripAndBounds(t *testing.T) {
	var buffer bytes.Buffer
	deadline := time.Unix(1_800_000_000, 0)
	if err := WriteGoawayNotice(&buffer, GoawayHostDraining, deadline); err != nil {
		t.Fatal(err)
	}
	if want := "\x00\x00\x00\x36" + `{"reason":"host_draining","drain_deadline":1800000000}`; buffer.String() != want {
		t.Fatalf("goaway notice = %q", buffer.String())
	}
	notice, err := ReadGoawayNotice(&buffer)
	if err != nil || notice.Reason != GoawayHostDraining || notice.DrainDeadline != deadline.Unix() {
		t.Fatalf("read notice = %+v, %v", notice, err)
	}
	for _, bad := range []string{`{"reason":"bored","drain_deadline":1}`, `{"reason":"edge_draining"}`, `{"reason":"edge_draining","drain_deadline":1,"x":1}`, `{"reason":"edge_draining","drain_deadline":1` + strings.Repeat(" ", 2048) + `}`} {
		if _, err := ReadGoawayNotice(strings.NewReader(goawayFrame(bad))); err == nil {
			t.Fatalf("accepted goaway notice %.60s", bad)
		}
	}
	if _, err := ReadGoawayNotice(strings.NewReader("\x00\x00\x10\x00")); err == nil {
		t.Fatal("accepted an oversized goaway frame")
	}
	if err := WriteGoawayNotice(&buffer, "bored", deadline); err == nil {
		t.Fatal("wrote a goaway notice with an unknown reason")
	}
	open := StreamOpen{Protocol: ProtocolName, Version: ProtocolVersion, AccountID: "account_a", TunnelID: "tunnel_a", ConnectorID: "connector_a", SessionID: "session_a", ProcessGeneration: 1, Generation: 1, RouteID: "route_a", RequestID: "request_a", Kind: Goaway}
	if err := open.Validate(); err != nil {
		t.Fatalf("goaway stream open rejected: %v", err)
	}
}

func goawayFrame(body string) string {
	n := len(body)
	return string([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}) + body
}
