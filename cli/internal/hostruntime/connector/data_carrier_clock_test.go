package connector

import (
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
	"net/http"
	"testing"
	"time"
)

func TestDataCarrierTargetsUseServerClock(t *testing.T) {
	local := time.Now()
	serverclock.Observe(local.Add(-10*time.Minute).UTC().Format(http.TimeFormat), local, local)
	t.Cleanup(func() { now := time.Now(); serverclock.Observe(now.UTC().Format(http.TimeFormat), now, now) })
	for _, refresh := range []bool{false, true} {
		for _, fresh := range []bool{true, false} {
			t.Run(fmt.Sprintf("refresh=%t/fresh=%t", refresh, fresh), func(t *testing.T) {
				config := DefaultDataCarrierPoolConfig()
				config.MaximumCarriers = 1
				config.Targets = []DataCarrierTarget{{EdgeID: "edge-a", ProcessEpoch: "epoch-a", FailureDomain: "domain-a"}}
				expiry := serverclock.Now().Add(-time.Second)
				if fresh {
					expiry = serverclock.Now().Add(2 * time.Minute)
				}
				config.TargetsExpireAt = expiry
				if refresh {
					config.TargetsExpireAt = serverclock.Now().Add(-time.Minute)
					config.RefreshTargets = func(context.Context) ([]DataCarrierTarget, time.Time, error) { return config.Targets, expiry, nil }
				}
				calls := 0
				dialErr := errors.New("dial reached")
				pool, err := NewDataCarrierPool(context.Background(), config, func(ctx context.Context, _ DataCarrierDialRequest) (DataCarrierDialResult, error) {
					calls++
					if ctx.Err() != nil {
						t.Errorf("fresh target got expired dial context: %v", ctx.Err())
					}
					if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 6*time.Second {
						t.Error("dial timeout is not bounded in local time")
					}
					return DataCarrierDialResult{}, dialErr
				})
				if err != nil {
					t.Fatal(err)
				}
				defer pool.Close()
				if refresh {
					pool.refreshedAt = time.Now().Add(-5 * time.Second)
				}
				err = pool.Connect(context.Background())
				if fresh && (calls == 0 || !errors.Is(err, dialErr)) {
					t.Fatalf("fresh target never dialed: calls=%d err=%v", calls, err)
				}
				if !fresh && calls != 0 {
					t.Fatalf("expired target dialed %d times", calls)
				}
			})
		}
	}
}
