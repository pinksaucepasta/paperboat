package nodelifecycle

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"github.com/pinksaucepasta/paperboat/shared/nodemetrics"
)

// An observation reports the relay's version and the payload bytes it forwarded (as bytes out), within the bounds the
// control plane enforces.
func TestObservationMetricsCarryVersionAndForwardedBytes(t *testing.T) {
	c := &Client{cfg: Config{Version: "dev-relay"}, sampler: nodemetrics.NewSampler()}
	got := c.metrics(derpquic.Stats{Forwarded: 3, BytesForwarded: 4096})
	if got.Version != "dev-relay" || got.BytesOut != 4096 || got.BytesIn != 0 {
		t.Fatalf("report = %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
}
