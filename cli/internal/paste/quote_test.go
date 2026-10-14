package paste

import (
	"bytes"
	transfer "github.com/pinksaucepasta/paperboat/internal/filetransfer"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

func TestQuoteRemotePathsAsLiteralArguments(t *testing.T) {
	for _, path := range []string{"/remote/safe.txt", "/remote/Paperboat Inbox/image (2).png", "/remote/a'b $HOME $(echo bad);*.txt", "/remote/你好.txt"} {
		quoted, err := quoteRemotePath(path)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS == "windows" {
			continue
		}
		output, err := exec.Command("sh", "-c", "set -- "+quoted+"; printf '%s\\000' \"$@\"").Output()
		if err != nil || !bytes.Equal(output, append([]byte(path), 0)) {
			t.Fatalf("path=%q output=%q err=%v", path, output, err)
		}
	}
	for _, path := range []string{`C:\Users\Pujan\Paperboat Inbox\image (2).png`, `C:\Users\Pujan\Paperboat Inbox\a'b & c.txt`, `\\host\share\file name.txt`} {
		got, err := quoteRemotePath(path)
		if err != nil || got != "\""+path+"\"" {
			t.Fatalf("path=%q got=%q err=%v", path, got, err)
		}
	}
	for _, path := range []string{`C:\Inbox\$name.txt`, `C:\Inbox\%name%.txt`, `C:\Inbox\a!b.txt`, `C:\Inbox\a` + "`" + `b.txt`, "relative/path", "/remote/new\nline"} {
		if _, err := quoteRemotePath(path); err == nil {
			t.Fatalf("unsafe path accepted: %q", path)
		}
	}
}

func TestUploadedPathQuotesPreservePasteFramingAndWhitespace(t *testing.T) {
	dir := t.TempDir()
	local := makeImage(t, dir, "local name.png")
	for _, remote := range []string{"/remote/Paperboat Inbox/image (2).png", `C:\Users\Pujan\Paperboat Inbox\image (2).png`} {
		var dest bytes.Buffer
		i := New(&dest, fixedUploader{remote}, defaultLimits(), WithPartialFlushDelay(0))
		writeInChunks(t, i, wrap("  '"+local+"'\t"), 3)
		quoted, _ := quoteRemotePath(remote)
		if got, want := dest.String(), wrap("  "+quoted+"\t"); got != want {
			t.Fatalf("got=%q want=%q", got, want)
		}
	}
}

func TestUnsafeWindowsPathKeepsEntireOriginalBatch(t *testing.T) {
	dir := t.TempDir()
	first := makeImage(t, dir, "first.png")
	second := makeImage(t, dir, "second.png")
	uploader := &batchUploader{result: transfer.Batch{Paths: []string{"/remote/valid file.png", `C:\Inbox\$name.png`}}}
	var dest, notice bytes.Buffer
	i := New(&dest, uploader, defaultLimits(), WithNotifier(&notice), WithPartialFlushDelay(0))
	original := first + "\n" + second
	writeInChunks(t, i, wrap(original), 5)
	if dest.String() != wrap(original) || !strings.Contains(notice.String(), "file uploaded, but its path could not be inserted safely") {
		t.Fatalf("output=%q notice=%q", dest.String(), notice.String())
	}
}
