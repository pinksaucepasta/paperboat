package filetransfer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func openFDCount(t *testing.T) (int, bool) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("fd counts only on linux (/proc/self/fd)")
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no /proc/self/fd: %v", err)
	}
	return len(entries), true
}

// tarWriterRunning reports whether any goroutine is inside writeFolderTar: precise where a goroutine count is not
// (HTTP servers and transports keep their own).
func tarWriterRunning() bool {
	buf := make([]byte, 1<<20)
	for {
		if n := runtime.Stack(buf, true); n < len(buf) {
			return strings.Contains(string(buf[:n]), "writeFolderTar")
		}
		buf = make([]byte, len(buf)*2) // truncated: a missed writer would read as "exited"
	}
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Abandoning a partially-read folder stream must stop its tar writer and
// release the in-flight fd even though nobody seeks again.
func TestFolderAbandonedStreamReleasesWriter(t *testing.T) {
	baseFDs, haveFDs := openFDCount(t)

	// A folder big enough that the writer blocks on a full pipe after a partial read.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "big.bin"), make([]byte, 8<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	batch, err := Prepare([]string{root}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Close()
	stream, ok := batch.Sources[0].Reader.(*folderStream)
	if !ok {
		t.Fatal("folder source is not a *folderStream")
	}
	if _, err := stream.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(io.Discard, stream, 1<<20); err != nil {
		t.Fatal(err)
	}
	// A read blocked on the writer when the abort comes (the transport still reading an abandoned body).
	blocked := make(chan error, 1)
	go func() { _, err := io.CopyN(io.Discard, stream, 4<<20); blocked <- err }()
	waitFor(t, "the reader to be inside the stream", 30*time.Second, func() bool { return stream.position() > 1<<20 })
	// Simulate abort: nobody will Seek again; Close must stop the writer.
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal("Close is not idempotent")
	}
	select {
	case <-blocked: // the reader was released by Close, with an error or what had already arrived
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not release a blocked Read")
	}
	// A read that only started after the first Close (a loaded machine) restarts the stream by design; end that too.
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "writer goroutine to exit", 5*time.Second, func() bool {
		return !tarWriterRunning()
	})
	if haveFDs {
		waitFor(t, "fds to return to baseline", 5*time.Second, func() bool {
			n, _ := os.ReadDir("/proc/self/fd")
			return len(n) <= baseFDs
		})
	}
	// The stream restarts from zero after an abort Close.
	if _, err := stream.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, stream); err != nil && err != io.EOF {
		// io.Copy returns nil on EOF; any other error fails the restart.
		t.Fatal(err)
	}
}

// uploadNative's terminal error path must close a folder source promptly:
// after a permanent PATCH refusal the writer is gone and the stream restarts.
func TestUploadNativeFolderAbortClosesStream(t *testing.T) {

	// Big enough that the tar writer is still blocked on the pipe when the refusal arrives.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "big.bin"), make([]byte, 48<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	batch, err := Prepare([]string{root}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Close()
	item := batch.Sources[0]

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead:
			w.Header().Set("Upload-Offset", "0")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPatch:
			// Permanent refusal: offset_conflict raised as a real conflict would be,
			// but here forced so uploadNative takes its terminal refusedOffset path.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"offset_conflict"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClient(server.URL+"/v1/file-transfers", Auth{Token: "token"}, testBinding(), server.Client())
	manifest := Manifest{TransferID: "ft_folder_abort", BatchID: "fb_folder_abort",
		SourceMachineID: "machine_source", DestinationMachineID: "machine_destination",
		InitiatingUserID: "user_1", Basename: item.Basename, Size: item.Size}
	err = client.uploadNative(context.Background(), manifest, item, -1)
	if err == nil || !strings.Contains(err.Error(), "offset_conflict") {
		t.Fatalf("uploadNative err = %v, want offset_conflict", err)
	}
	// The defer in uploadNative closed the folder: the writer is gone ...
	waitFor(t, "folder writer to exit after abort", 5*time.Second, func() bool {
		return !tarWriterRunning()
	})
	// ... and the stream restarts cleanly for the resume.
	if _, err := item.Reader.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek after abort: %v", err)
	}
	head := make([]byte, 512)
	if _, err := io.ReadFull(item.Reader, head); err != nil {
		t.Fatalf("read after abort: %v", err)
	}
}
