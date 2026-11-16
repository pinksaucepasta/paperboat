package filetransfer

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

type tarItem struct {
	name string
	body string
	typ  byte
	link string
}

func buildTar(t *testing.T, items ...tarItem) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := tar.NewWriter(&buffer)
	for _, item := range items {
		header := &tar.Header{Name: item.name, Typeflag: item.typ, Mode: 0o4777, Size: int64(len(item.body)), Linkname: item.link}
		if item.typ == 0 {
			header.Typeflag = tar.TypeReg
		}
		if header.Typeflag == tar.TypeDir || header.Typeflag == tar.TypeSymlink {
			header.Size = 0
		}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := archive.Write([]byte(item.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func goodTar(t *testing.T) []byte {
	return buildTar(t,
		tarItem{name: "docs/", typ: tar.TypeDir}, tarItem{name: "docs/a.txt", body: "alpha"},
		tarItem{name: "docs/deep/", typ: tar.TypeDir}, tarItem{name: "docs/deep/ünï.txt", body: "unicode"},
		tarItem{name: "empty/", typ: tar.TypeDir}, tarItem{name: "CON.txt", body: "reserved"})
}

// sendFolder creates a folder transfer holding data and uploads it in two chunks (a resume in the middle).
func sendFolder(t *testing.T, service *Service, data []byte) (string, error) {
	t.Helper()
	request := requestFor(data)
	request.Files[0].Basename = "proj"
	request.Files[0].Directory = true
	created, err := service.Create(context.Background(), request)
	if err != nil {
		return "", err
	}
	id := created[0].ID
	half := int64(len(data) / 2)
	if _, err := service.Append(context.Background(), id, 0, bytes.NewReader(data[:half])); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Append(context.Background(), id, half, bytes.NewReader(data[half:])); err != nil {
		t.Fatal(err)
	}
	_, err = service.Complete(context.Background(), id)
	return id, err
}

func spoolIsEmpty(t *testing.T, spool string) {
	t.Helper()
	entries, err := os.ReadDir(spool)
	if err != nil || len(entries) != 0 {
		t.Fatalf("the spool still holds %v (err %v)", entries, err)
	}
}

func TestFolderIsPublishedAsAFolderWithEmptyFoldersAndPrivateModes(t *testing.T) {
	service, _, spool := newService(t)
	inbox := t.TempDir()
	service.config.PublishRoot = func() (string, error) { return inbox, nil }
	id, err := sendFolder(t, service, goodTar(t))
	if err != nil {
		t.Fatal(err)
	}
	path, err := service.PublishedPath(context.Background(), id)
	if err != nil || path != filepath.Join(inbox, "proj") {
		t.Fatalf("PublishedPath = %q, %v", path, err)
	}
	for rel, body := range map[string]string{"docs/a.txt": "alpha", "docs/deep/ünï.txt": "unicode", "CON.txt": "reserved"} {
		got, err := os.ReadFile(filepath.Join(path, filepath.FromSlash(rel)))
		if err != nil || string(got) != body {
			t.Fatalf("%s = %q, %v", rel, got, err)
		}
	}
	if info, err := os.Stat(filepath.Join(path, "empty")); err != nil || !info.IsDir() {
		t.Fatalf("the empty folder was not kept: %v", err)
	}
	if runtime.GOOS != "windows" {
		for rel, want := range map[string]os.FileMode{"CON.txt": 0o600, "docs/a.txt": 0o600, "docs": 0o700, "empty": 0o700} {
			if info, err := os.Stat(filepath.Join(path, rel)); err != nil || info.Mode().Perm() != want {
				t.Fatalf("%s mode = %v (err %v), want %v: the sender's setuid/exec bits must not arrive", rel, info.Mode(), err, want)
			}
		}
	}
	if existing, err := service.ExistingPublishedPath(context.Background(), id); err != nil || existing != path {
		t.Fatalf("ExistingPublishedPath = %q, %v", existing, err)
	}
	if _, err := os.Stat(filepath.Join(inbox, "."+id+".publish")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the hidden unpack folder is left: %v", err)
	}
	if _, err := os.Stat(filepath.Join(spool, id+".content")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the spooled tar is left after publication: %v", err)
	}
	if again, err := service.PublishedPath(context.Background(), id); err != nil || again != path {
		t.Fatalf("a repeated publication = %q, %v", again, err)
	}
}

func TestFolderNameTakenInTheInboxIsNumberedLikeAFile(t *testing.T) {
	service, _, _ := newService(t)
	inbox := t.TempDir()
	if err := os.Mkdir(filepath.Join(inbox, "proj"), 0o700); err != nil {
		t.Fatal(err)
	}
	service.config.PublishRoot = func() (string, error) { return inbox, nil }
	id, err := sendFolder(t, service, goodTar(t))
	if err != nil {
		t.Fatal(err)
	}
	path, err := service.PublishedPath(context.Background(), id)
	if err != nil || path != filepath.Join(inbox, "proj (2)") {
		t.Fatalf("PublishedPath = %q, %v", path, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(inbox, "proj")); len(entries) != 0 {
		t.Fatal("the folder that was already there was written into")
	}
}

func TestFolderThatTheReceiverCannotStoreIsRefusedWholeAndLeavesNothing(t *testing.T) {
	cases := map[string]struct {
		platform string
		items    []tarItem
		want     string
	}{
		"symbolic link":        {"linux", []tarItem{{name: "l", typ: tar.TypeSymlink, link: "/etc/passwd"}}, "not links"},
		"hard link":            {"linux", []tarItem{{name: "a", body: "x"}, {name: "h", typ: tar.TypeLink, link: "a"}}, "not links"},
		"parent traversal":     {"linux", []tarItem{{name: "d/", typ: tar.TypeDir}, {name: "d/../../x", body: "x"}}, "directory reference"},
		"absolute":             {"linux", []tarItem{{name: "/etc/x", body: "x"}}, "empty"},
		"before its folder":    {"linux", []tarItem{{name: "d/f", body: "x"}}, "before its folder"},
		"under a file":         {"linux", []tarItem{{name: "f", body: "x"}, {name: "f/g", body: "y"}}, "before its folder"},
		"duplicate":            {"linux", []tarItem{{name: "f", body: "x"}, {name: "f", body: "y"}}, "twice"},
		"backslash":            {"linux", []tarItem{{name: "a\\b", body: "x"}}, "Team Inbox"},
		"windows reserved":     {"windows", []tarItem{{name: "d/", typ: tar.TypeDir}, {name: "d/aux.txt", body: "x"}}, "reserved Windows device name"},
		"windows trailing dot": {"windows", []tarItem{{name: "x.", body: "x"}}, "silently removes"},
		"windows colon":        {"windows", []tarItem{{name: "a:b", body: "x"}}, "does not allow"},
		"case collision":       {"windows", []tarItem{{name: "A.txt", body: "x"}, {name: "a.txt", body: "y"}}, "twice"},
		"case collision mac":   {"darwin", []tarItem{{name: "d/", typ: tar.TypeDir}, {name: "D/", typ: tar.TypeDir}}, "twice"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			service, durable, spool := newService(t)
			service.config.Platform = tc.platform
			inbox := t.TempDir()
			service.config.PublishRoot = func() (string, error) { return inbox, nil }
			id, err := sendFolder(t, service, buildTar(t, tc.items...))
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != InvalidPath || !strings.Contains(failure.Cause.Error(), tc.want) {
				t.Fatalf("Complete = %v, want invalid_path containing %q", err, tc.want)
			}
			if entries, _ := os.ReadDir(inbox); len(entries) != 0 {
				t.Fatalf("the Inbox holds %v", entries)
			}
			spoolIsEmpty(t, spool)
			if record, err := durable.FileTransfer(context.Background(), id); err != nil || record.State != "canceled" {
				t.Fatalf("record = %+v, %v", record, err)
			}
		})
	}
}

func TestFolderWithTooManyEntriesIsRefused(t *testing.T) {
	old := folderEntryLimit
	folderEntryLimit = 2
	defer func() { folderEntryLimit = old }()
	service, _, spool := newService(t)
	service.config.PublishRoot = func() (string, error) { return t.TempDir(), nil }
	_, err := sendFolder(t, service, buildTar(t, tarItem{name: "a", body: "1"}, tarItem{name: "b", body: "2"}, tarItem{name: "c", body: "3"}))
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != InvalidPath || !strings.Contains(failure.Cause.Error(), "more than 2") {
		t.Fatalf("Complete = %v", err)
	}
	spoolIsEmpty(t, spool)
}

func TestFolderCopyOnAFullDiskLeavesNoPartialFolderAndPublishesOnRetry(t *testing.T) {
	service, durable, spool := newService(t)
	inbox := t.TempDir()
	service.config.PublishRoot = func() (string, error) { return inbox, nil }
	created := 0
	createUnpacked = func(path string) (*os.File, error) {
		if created++; created == 2 {
			return nil, syscall.ENOSPC
		}
		return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	}
	defer func() {
		createUnpacked = func(path string) (*os.File, error) {
			return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		}
	}()
	id, err := sendFolder(t, service, goodTar(t))
	if err != nil {
		t.Fatal(err)
	}
	var failure *Error
	if _, err := service.PublishedPath(context.Background(), id); !errors.As(err, &failure) || failure.Code != InsufficientStorage {
		t.Fatalf("PublishedPath on a full disk = %v", err)
	}
	entries, _ := os.ReadDir(inbox)
	if len(entries) != 0 {
		t.Fatalf("a partial folder is left in the Inbox: %v", entries)
	}
	if record, _ := durable.FileTransfer(context.Background(), id); record.State != "failed" || record.ResultCode != string(InsufficientStorage) {
		t.Fatalf("record = %+v", record)
	}
	if _, err := os.Stat(filepath.Join(spool, id+".content")); err != nil {
		t.Fatalf("the tar was dropped, a repeated complete has nothing to publish: %v", err)
	}
	created = 100 // the disk has room again
	if _, err := service.Complete(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	path, err := service.PublishedPath(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(path, "docs", "a.txt")); err != nil || string(got) != "alpha" {
		t.Fatalf("the retried folder = %q, %v", got, err)
	}
}

func TestExpiredFolderIsRemovedFromTheSpoolWhenThereIsNoInbox(t *testing.T) {
	service, _, spool := newService(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	service.config.Now = func() time.Time { return now }
	id, err := sendFolder(t, service, goodTar(t))
	if err != nil {
		t.Fatal(err)
	}
	path, err := service.PublishedPath(context.Background(), id)
	if err != nil || filepath.Dir(path) != spool {
		t.Fatalf("PublishedPath = %q, %v", path, err)
	}
	now = now.Add(365 * 24 * time.Hour)
	if err := service.CleanupExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	spoolIsEmpty(t, spool)
}

func TestUnfinishedFolderUploadIsCanceledWithNothingLeft(t *testing.T) {
	service, _, spool := newService(t)
	data := goodTar(t)
	request := requestFor(data)
	request.Files[0].Directory = true
	created, err := service.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Append(context.Background(), created[0].ID, 0, bytes.NewReader(data[:100])); err != nil {
		t.Fatal(err)
	}
	if err := service.cancel(context.Background(), created[0].ID); err != nil {
		t.Fatal(err)
	}
	spoolIsEmpty(t, spool)
}

func TestFolderIsNotAcceptedForARelayedDestination(t *testing.T) {
	service, _, _ := newService(t)
	request := relayRequest(goodTar(t))
	request.Files[0].Directory = true
	var failure *Error
	if _, err := service.Create(context.Background(), request); !errors.As(err, &failure) || failure.Code != InvalidPath {
		t.Fatalf("Create = %v", err)
	}
}

func TestFileKeepsItsPathWhenFoldersExist(t *testing.T) {
	service, _, _ := newService(t)
	inbox := t.TempDir()
	service.config.PublishRoot = func() (string, error) { return inbox, nil }
	data := []byte("plain file that happens to be a tar-looking blob")
	created, err := service.Create(context.Background(), requestFor(data))
	if err != nil {
		t.Fatal(err)
	}
	id := created[0].ID
	if _, err := service.Append(context.Background(), id, 0, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Complete(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	path, err := service.PublishedPath(context.Background(), id)
	if err != nil || path != filepath.Join(inbox, "data.bin") {
		t.Fatalf("PublishedPath = %q, %v", path, err)
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("a single file must stay a file: %v", err)
	}
}
