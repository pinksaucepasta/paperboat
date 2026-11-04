//go:build linux

package bandwidth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Freeze only a disposable, separately mounted filesystem: this exercises the
// actual atomic-file/fsync path without injecting a persistence implementation.
func TestMeteredWriteContinuesWhileCheckpointFilesystemIsFrozen(t *testing.T) {
	mount, unfreeze := frozenRecorderFilesystem(t)
	sink := &testSink{}
	recorder, err := Open(filepath.Join(mount, "usage.json"), sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		unfreeze()
		if err := recorder.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	writer, reader := net.Pipe()
	defer writer.Close()
	defer reader.Close()
	received := make(chan error, 1)
	go func() { _, err := io.CopyN(io.Discard, reader, 6); received <- err }()
	metered := recorder.Wrap(writer, Binding{AccessSessionID: "access", StreamID: "stream", Consumer: "terminal"}, func(string) Path { return Path{Mode: "direct"} })
	if _, err := metered.Write([]byte("ab")); err != nil {
		t.Fatal(err)
	}
	freezeRecorderFilesystem(t, mount)
	tid := make(chan int, 1)
	flushed := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		tid <- syscall.Gettid()
		flushed <- recorder.Flush(t.Context())
	}()
	waitFrozenRecorderThread(t, <-tid)
	written := make(chan error, 1)
	go func() { _, err := metered.Write([]byte("cdef")); written <- err }()
	blocked := false
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		blocked = true
	}
	unfreeze()
	select {
	case err := <-flushed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("checkpoint did not resume after thaw")
	}
	if blocked {
		<-written
		t.Fatal("metered terminal write waited for checkpoint filesystem I/O after delivering its bytes")
	}
	if err := <-received; err != nil {
		t.Fatal(err)
	}
	// The first report contains only its durable snapshot; concurrent bytes stay
	// dirty and are delivered by the next flush with the same absolute identity.
	sink.mu.Lock()
	var first int64
	for _, v := range sink.reports {
		first += v.DownloadBytes
	}
	sink.mu.Unlock()
	if first != 2 {
		t.Fatalf("reported bytes beyond durable snapshot: %d", first)
	}
	if err := recorder.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(mount, "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state State
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, v := range state.Counters {
		total += v.Report.DownloadBytes
		if v.Dirty {
			t.Fatal("acknowledged counter remained dirty")
		}
	}
	if total != 6 {
		t.Fatalf("concurrent checkpoint lost or duplicated bytes: %d", total)
	}
}

func frozenRecorderFilesystem(t *testing.T) (string, func()) {
	t.Helper()
	root := t.TempDir()
	image := filepath.Join(root, "disk.img")
	mount := filepath.Join(root, "mounted")
	f, err := os.Create(image)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(64 << 20); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mount, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"mkfs.ext4", "-q", "-F", image}, {"mount", "-o", "loop", image, mount}} {
		if out, err := recorderFilesystemCommand(args...).CombinedOutput(); err != nil {
			t.Fatalf("isolated filesystem setup: %v: %s", err, out)
		}
	}
	t.Cleanup(func() {
		if out, err := recorderFilesystemCommand("umount", mount).CombinedOutput(); err != nil {
			t.Errorf("unmount disposable filesystem: %v: %s", err, out)
		}
	})
	if out, err := recorderFilesystemCommand("chown", fmt.Sprintf("%d:%d", os.Geteuid(), os.Getegid()), mount).CombinedOutput(); err != nil {
		t.Fatalf("set disposable filesystem owner: %v: %s", err, out)
	}
	var once sync.Once
	unfreeze := func() {
		once.Do(func() {
			if out, err := recorderFilesystemCommand("fsfreeze", "--unfreeze", mount).CombinedOutput(); err != nil {
				t.Errorf("thaw disposable filesystem: %v: %s", err, out)
			}
		})
	}
	var parent, child syscall.Statfs_t
	if err := syscall.Statfs(root, &parent); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Statfs(mount, &child); err != nil {
		t.Fatal(err)
	}
	if child.Fsid == parent.Fsid || child.Type != 0xef53 {
		t.Fatal("refusing to freeze anything except the separate test ext4 mount")
	}
	return mount, unfreeze
}
func freezeRecorderFilesystem(t *testing.T, mount string) {
	t.Helper()
	if out, err := recorderFilesystemCommand("fsfreeze", "--freeze", mount).CombinedOutput(); err != nil {
		t.Fatalf("freeze disposable filesystem: %v: %s", err, out)
	}
}
func waitFrozenRecorderThread(t *testing.T, tid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(fmt.Sprintf("/proc/self/task/%d/stat", tid))
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(string(body))
		if len(fields) > 2 && fields[2] == "D" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("checkpoint did not reach the frozen filesystem")
}

func recorderFilesystemCommand(args ...string) *exec.Cmd {
	if os.Geteuid() == 0 {
		return exec.Command(args[0], args[1:]...)
	}
	return exec.Command("sudo", append([]string{"-n"}, args...)...)
}
