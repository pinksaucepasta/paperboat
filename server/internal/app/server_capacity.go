package app

import (
	"context"
	"os"
	"strconv"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/shared/nodemetrics"
)

// serverCapacityRecorder records this server process on the operator capacity page: its CPU, memory and version
// (it counts no bytes or connections the way an edge does). The node id is the configured instance name, else the host name.
func serverCapacityRecorder(store *db.DB, instance string) func(context.Context) error {
	if instance == "" {
		instance, _ = os.Hostname()
	}
	if instance == "" {
		instance = "unnamed"
	}
	node, epoch, sampler := "server-"+instance, strconv.FormatInt(time.Now().Unix(), 10), nodemetrics.NewSampler()
	return func(ctx context.Context) error {
		cpu, rss := sampler.Sample(time.Now())
		return controlplane.RecordNodeCapacity(ctx, store, controlplane.NodeSample{
			NodeID: node, Role: "server", ProcessEpoch: epoch,
			Metrics: nodemetrics.Report{Version: buildinfo.Version, CPUPercent: cpu, RSSBytes: rss},
		}, time.Now())
	}
}
