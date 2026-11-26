package session

import (
	"context"
	"testing"
	"time"

	transfer "github.com/pinksaucepasta/paperboat/cli/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/cli/internal/paste"
)

type dropUploader string

func (u dropUploader) SendBatch(context.Context, string, string, []transfer.Source) (transfer.Batch, error) {
	return transfer.Batch{Paths: []string{string(u)}}, nil
}

// dropPolicy uploads every dropped file as remotePath; the channel reports each one that reached the remote.
func dropPolicy(remotePath string) (*paste.Policy, paste.Option, <-chan struct{}) {
	delivered := make(chan struct{}, 1)
	limits := transfer.Limits{MaxFileBytes: transfer.AutomaticLimit, MaxBatchFiles: 8, MaxBatchBytes: transfer.AutomaticLimit}
	return paste.NewPolicy(dropUploader(remotePath), "ses_test", limits), paste.WithLifecycle(func(event paste.LifecycleEvent) {
		if event == paste.FileComplete {
			delivered <- struct{}{}
		}
	}), delivered
}

func waitUntil(t *testing.T, what func() string, ready func() bool) {
	t.Helper()
	for end := time.Now().Add(30 * time.Second); !ready(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what())
		}
	}
}
