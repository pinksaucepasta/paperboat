//go:build darwin || linux

package hostinstall

import (
	"bytes"
	"testing"
)

type countingSyncer struct {
	bytes.Buffer
	syncs     int
	unsynced  int
	maxUnsync int
}

func (c *countingSyncer) Write(p []byte) (int, error) {
	c.unsynced += len(p)
	c.maxUnsync = max(c.maxUnsync, c.unsynced)
	return c.Buffer.Write(p)
}
func (c *countingSyncer) Sync() error { c.syncs++; c.unsynced = 0; return nil }

// dadape direct2c: one unsynced 90 MB stage stalled every terminal for 6 s.
func TestStagedBinaryCopyNeverLeavesMoreThanOneChunkUnsynced(t *testing.T) {
	src := bytes.Repeat([]byte{7}, 3*stageSyncBytes+123)
	var dst countingSyncer
	n, err := syncedCopy(&dst, bytes.NewReader(src))
	if err != nil || n != int64(len(src)) || !bytes.Equal(dst.Bytes(), src) {
		t.Fatalf("copy n=%d err=%v", n, err)
	}
	if dst.syncs != 3 || dst.maxUnsync > stageSyncBytes {
		t.Fatalf("syncs=%d max unsynced=%d", dst.syncs, dst.maxUnsync)
	}
}
