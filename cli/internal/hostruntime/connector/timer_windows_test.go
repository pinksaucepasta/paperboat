//go:build windows

package connector

import "golang.org/x/sys/windows"

// The throughput test's delay line sleeps per packet. At Windows' default 15.6 ms timer tick it releases the packets
// in bursts, the jitter trips quic-go's HyStart and a stream gets 100-140 Mbit/s instead of 300. The system timer rate
// is global, so another process asking for 1 ms (a browser, a VM) hides it. A real path has no timer tick.
func init() { _ = windows.TimeBeginPeriod(1) }
