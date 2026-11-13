package main

import (
	"strings"
	"testing"

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
