package peerquic

import "testing"

// The receive windows must cover the bandwidth-delay product of a wide-area path from the first round trip.
func TestReceiveWindowsCoverAWideAreaPath(t *testing.T) {
	for _, class := range []Class{ClassInteractive, ClassTransfer, ClassPreview} {
		config := quicConfig(DevelopmentSessionConfig(class))
		if config.InitialStreamReceiveWindow < 4<<20 || config.MaxStreamReceiveWindow < config.InitialStreamReceiveWindow || config.InitialConnectionReceiveWindow < config.InitialStreamReceiveWindow || config.MaxConnectionReceiveWindow < config.MaxStreamReceiveWindow {
			t.Fatalf("class %v: windows stream %d/%d connection %d/%d", class, config.InitialStreamReceiveWindow, config.MaxStreamReceiveWindow, config.InitialConnectionReceiveWindow, config.MaxConnectionReceiveWindow)
		}
	}
}
