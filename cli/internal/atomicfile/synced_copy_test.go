package atomicfile

import (
	"bytes"
	"testing"
)

type countingSyncer struct {
	bytes.Buffer
	syncs, unsynced, maxUnsynced int
}

func (c *countingSyncer) Write(p []byte) (int, error) {
	c.unsynced += len(p)
	c.maxUnsynced = max(c.maxUnsynced, c.unsynced)
	return c.Buffer.Write(p)
}
func (c *countingSyncer) Sync() error { c.syncs++; c.unsynced = 0; return nil }

// dadape direct2c/direct1d: one unsynced 90 MB copy stalled every terminal for 6 s.
func TestSyncedCopyNeverLeavesMoreThanOneChunkUnsynced(t *testing.T) {
	src := bytes.Repeat([]byte{7}, 3*SyncBytes+123)
	var dst countingSyncer
	n, err := SyncedCopy(&dst, bytes.NewReader(src))
	if err != nil || n != int64(len(src)) || !bytes.Equal(dst.Bytes(), src) {
		t.Fatalf("copy n=%d err=%v", n, err)
	}
	if dst.syncs != 3 || dst.maxUnsynced > SyncBytes {
		t.Fatalf("syncs=%d max unsynced=%d", dst.syncs, dst.maxUnsynced)
	}
}
