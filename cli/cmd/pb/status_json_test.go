package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
)

func TestStatusJSONShowsTheHomeRelayRegionOnly(t *testing.T) {
	snapshot := localapi.Snapshot{Schema: localapi.SnapshotSchemaV1, DaemonState: "ready",
		HomeRelay: &localapi.HomeRelay{NodeID: "relay_fra_2", Region: "eu-central"}}
	var out bytes.Buffer
	if err := writeStatusJSON(&out, snapshot); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if string(decoded["home_relay"]) != `{"region":"eu-central"}` {
		t.Fatalf("home_relay = %s", decoded["home_relay"])
	}
	if strings.Contains(out.String(), "node_id") || strings.Contains(out.String(), "relay_fra_2") {
		t.Fatalf("pb status --json shows a relay node id: %s", out.String())
	}
	if snapshot.HomeRelay.NodeID != "relay_fra_2" {
		t.Fatal("the daemon's snapshot lost the node id")
	}
	out.Reset()
	snapshot.HomeRelay = nil
	if err := writeStatusJSON(&out, snapshot); err != nil || strings.Contains(out.String(), "home_relay") {
		t.Fatalf("no home relay: err=%v %s", err, out.String())
	}
}
