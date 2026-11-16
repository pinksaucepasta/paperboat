package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/filetransfer"
)

func TestSendRefusesNamesTheReceiverCannotStoreBeforeSending(t *testing.T) {
	sources := []filetransfer.Source{{Basename: "ok.txt"}, {Basename: "a:b.txt"}, {Basename: "trail."}, {Basename: "CON.txt"}}
	err := receiverNameRefusal("winbox", "windows", sources)
	if err == nil {
		t.Fatal("a Windows receiver was offered names it cannot store")
	}
	for _, want := range []string{"winbox", `"a:b.txt"`, `"trail."`, `"CON.txt"`, "Nothing was sent", "Rename the files"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err.Error(), want)
		}
	}
	if printsUsage(err) {
		t.Errorf("a refused name is not wrong arguments: the command help must not follow the message")
	}
	if strings.Contains(err.Error(), `"ok.txt"`) {
		t.Errorf("a storable name is listed: %q", err.Error())
	}
	if err := receiverNameRefusal("lbox", "linux", sources); err != nil {
		t.Fatalf("a Linux receiver stores these names: %v", err)
	}
}

func TestSendRefusesSurroundingWhiteSpaceForEveryReceiver(t *testing.T) {
	err := receiverNameRefusal("macbox", "darwin", []filetransfer.Source{{Basename: "sp "}})
	if err == nil || !strings.Contains(err.Error(), `"sp "`) || !strings.Contains(err.Error(), "Nothing was sent") {
		t.Fatalf("the Team Inbox refuses this name for every receiver, the message must name it: %v", err)
	}
}

func TestSendRefusesAFolderWhoseFilesTheReceiverCannotStore(t *testing.T) {
	folder := filetransfer.Source{Basename: "proj", Folder: &filetransfer.Folder{Paths: []string{"src", "src/ok.txt", "src/CON.txt", "docs", "docs/Readme.md", "docs/README.md"}}}
	err := receiverNameRefusal("winbox", "windows", []filetransfer.Source{folder})
	if err == nil {
		t.Fatal("a Windows receiver was offered a folder it cannot store")
	}
	for _, want := range []string{"winbox", `"proj/src/CON.txt"`, "reserved Windows device name", `in "proj"`, "Readme.md", "Nothing was sent"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "ok.txt") {
		t.Errorf("a storable path is listed: %q", err.Error())
	}
	if err := receiverNameRefusal("lbox", "linux", []filetransfer.Source{folder}); err != nil {
		t.Fatalf("a Linux receiver stores this folder: %v", err)
	}
}

func TestSendFolderNoticeNamesWhatIsSentAndWhatIsNot(t *testing.T) {
	source := filetransfer.Source{Basename: "proj", Size: 3 << 20, Folder: &filetransfer.Folder{Paths: []string{"a", "a/b.txt", "c.txt"}, Files: 2, Skipped: []filetransfer.SkippedEntry{{Path: "a/link", Kind: "symbolic link"}}}}
	var out strings.Builder
	folderNotice(&out, []filetransfer.Source{{Basename: "plain.txt"}, source})
	for _, want := range []string{"Folder proj: 2 files, 1 folders, 3 MiB", "not sent: proj/a/link (symbolic link"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("notice %q lacks %q", out.String(), want)
		}
	}
	if err := folderToThisComputer([]filetransfer.Source{source}); err == nil || !strings.Contains(err.Error(), "proj is a folder") {
		t.Fatalf("a folder to this computer = %v", err)
	}
	if err := folderToThisComputer([]filetransfer.Source{{Basename: "plain.txt"}}); err != nil {
		t.Fatalf("a file to this computer = %v", err)
	}
	if got := folderSuffix(source); got != " (folder, 2 files)" || folderSuffix(filetransfer.Source{}) != "" {
		t.Fatalf("suffix = %q", got)
	}
}

func TestSendNamesAFileThatChangedWhileItsFolderWasSent(t *testing.T) {
	wrapped := fmt.Errorf("send batch: %w", errors.Join(errors.New("file transfer source could not be read"), &filetransfer.SourceChangedError{Path: "/home/u/proj/a.txt"}))
	got := folderChanged(wrapped)
	if !errors.Is(got, errUsage) || !strings.Contains(got.Error(), "/home/u/proj/a.txt changed while the folder was being sent") || !strings.Contains(got.Error(), "nothing was delivered") || printsUsage(got) {
		t.Fatalf("message = %v", got)
	}
	other := errors.New("boom")
	if folderChanged(other) != other {
		t.Fatal("another error was rewritten")
	}
}

func TestApproveRefusesStoredNamesThisOSCannotStore(t *testing.T) {
	files := []api.TeamInboxFile{{Basename: "ok.txt"}, {Basename: "CON.txt"}}
	err := approveNameRefusal("windows", files)
	if err == nil {
		t.Fatal("a Windows machine approved a file it cannot store: the request would stay approved and never arrive")
	}
	for _, want := range []string{`"CON.txt"`, "reserved", "Not approved", "ask the sender to rename"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), `"ok.txt"`) {
		t.Errorf("message names a file that is fine: %q", err.Error())
	}
	if err := approveNameRefusal("linux", files); err != nil {
		t.Fatalf("a Linux machine refused names it can store: %v", err)
	}
}
