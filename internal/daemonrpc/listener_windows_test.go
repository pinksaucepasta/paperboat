//go:build windows

package daemonrpc

import (
	"fmt"
	"testing"
	"time"
)

func testSocket(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s-test-%d", DefaultSocketAddress(), time.Now().UnixNano())
}
