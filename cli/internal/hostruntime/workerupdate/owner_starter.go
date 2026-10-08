package workerupdate

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
)

// OwnerStarter leaves process lifetime and confirmed teardown with native
// hostd. Cancelling the updater cannot kill an activated feature worker.
type OwnerStarter struct {
	Client hostdproto.WorkerControlClient
}

func (s OwnerStarter) Start(ctx context.Context, r StartRequest) (Worker, error) {
	if s.Client == nil || !r.MutationsDisabled {
		return nil, ErrInvalidConfig
	}
	_, err := hostdproto.ControlWorker(ctx, s.Client, hostdproto.WorkerControlRequest{Operation: "start", WorkerID: r.WorkerID, Executable: r.Executable, Version: r.Release.Version, APIMin: r.Release.HostdAPIMin, APIMax: r.Release.HostdAPIMax})
	if err != nil {
		return nil, err
	}
	return &ownerWorker{client: s.Client, id: r.WorkerID}, nil
}

type ownerWorker struct {
	client hostdproto.WorkerControlClient
	id     string
}

func (w *ownerWorker) status(ctx context.Context, op string) (hostdproto.Status, error) {
	r, err := hostdproto.ControlWorker(ctx, w.client, hostdproto.WorkerControlRequest{Operation: op, WorkerID: w.id})
	if err != nil {
		return hostdproto.Status{}, err
	}
	if r.Status == nil {
		return hostdproto.Status{}, hostdproto.ErrInvalidFrame
	}
	return *r.Status, nil
}
func (w *ownerWorker) Ready(ctx context.Context) (hostdproto.Status, error) {
	return w.status(ctx, "ready")
}
func (w *ownerWorker) Activate(ctx context.Context) (hostdproto.Status, error) {
	return w.status(ctx, "activate")
}
func (w *ownerWorker) Stop(ctx context.Context) error {
	_, err := hostdproto.ControlWorker(ctx, w.client, hostdproto.WorkerControlRequest{Operation: "stop", WorkerID: w.id})
	return err
}
