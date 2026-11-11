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
