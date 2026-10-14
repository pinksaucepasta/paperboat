//go:build darwin || linux || windows

package runtime

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/bandwidth"
	"sync"
	"time"
)

// hostBandwidthService owns one durable recorder across native generations and
// browser attachments. The stable host starts it before listeners and drains
// it after those listeners and their application handlers have stopped.
type hostBandwidthService struct {
	recorder *bandwidth.Recorder
	once     sync.Once
	err      error
}

func (s *hostBandwidthService) Start(context.Context) error { return nil }
func (s *hostBandwidthService) Shutdown(ctx context.Context) error {
	s.once.Do(func() {
		if s.recorder == nil {
			return
		}
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		s.err = s.recorder.Close(closeCtx)
	})
	return s.err
}
