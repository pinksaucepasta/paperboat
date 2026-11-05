//go:build darwin || linux || windows

package runtime

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/bandwidth"
)

type carrierUsageSink struct {
	mu      sync.Mutex
	reports []api.NativeUsageReport
}

func (s *carrierUsageSink) ReportNativeUsage(_ context.Context, reports []api.NativeUsageReport, _, _ int64, _ bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports = append(s.reports, reports...)
	return nil
}

// An SSH session served until the update released the network is metered by that carrier. Its replacement
// runs in another slot and never reopens the released one's usage file, so the released carrier delivers the session's
// last bytes when it exits; a carrier that is only restarting checkpoints them for the next process in its slot.
func TestReleasedCarrierDeliversSessionUsageAtExit(t *testing.T) {
	for _, retired := range []bool{true, false} {
		sink := &carrierUsageSink{}
		recorder, err := bandwidth.Open(filepath.Join(t.TempDir(), "carrier-0-bandwidth-usage.json"), sink, nil)
		if err != nil {
			t.Fatal(err)
		}
		left, right := net.Pipe()
		defer right.Close()
		metered := recorder.Wrap(left, bandwidth.Binding{AccessSessionID: "machine_host", StreamID: "stream_ssh", Consumer: "ssh", PeerAccountID: "account_peer"}, func(string) bandwidth.Path { return bandwidth.Path{Mode: "relay", NodeID: "relay_1"} })
		go func() { _, _ = io.Copy(io.Discard, right) }()
		if _, err := metered.Write(make([]byte, 1<<20)); err != nil {
			t.Fatal(err)
		}
		carrier := &ProductionCarrier{peer: &productionNativePeerService{}, authorization: serviceGroup{}, usage: &hostBandwidthService{recorder: recorder}, released: retired}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		if err := carrier.Shutdown(ctx); err != nil {
			t.Fatal(err)
		}
		cancel()
		sink.mu.Lock()
		var down int64
		for _, r := range sink.reports {
			if r.Consumer == "ssh" && r.PeerAccountID == "account_peer" {
				down += r.DownloadBytes
			}
		}
		sink.mu.Unlock()
		if want := map[bool]int64{true: 1 << 20, false: 0}[retired]; down != want {
			t.Fatalf("retired=%v: carrier exit delivered %d ssh bytes, want %d", retired, down, want)
		}
	}
}
