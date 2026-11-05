//go:build darwin

package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/privateproxyconfig"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
	"github.com/pinksaucepasta/paperboat/internal/privatepreviewproxy"
)

// newPlatformPrivatePAC registers hostd's PAC with the machine guard over one long-lived connection (the guard
// drops it when the connection closes).
func newPlatformPrivatePAC(string) (privatepreviewproxy.PACConfigurator, error) {
	var mu sync.Mutex
	var client *machineguard.Client
	set := func(ctx context.Context, body string) error {
		mu.Lock()
		defer mu.Unlock()
		if client == nil {
			c, err := machineguard.Connect(ctx, "")
			if err != nil {
				return errors.Join(privateproxyconfig.ErrUnsupported, fmt.Errorf("machine guard is not reachable: %w", err))
			}
			client = c
		}
		err := client.SetPreviewPAC(ctx, body)
		if err != nil || body == "" {
			err = errors.Join(err, client.Close())
			client = nil
		}
		return err
	}
	return guardPAC{set: set, fetch: fetchLoopbackPAC}, nil
}
