package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

// After an unreadable lifecycle state the cleanup removes everything by known names; `pb uninstall --json` carries
// that list (agents read the JSON, not stderr) and stays free of the field when nothing was removed that way.
func TestUninstallJSONResultCarriesWhatWasRemovedByName(t *testing.T) {
	removed := []string{"/etc/systemd/system/paperboat-hostd-u1000.service", "process 4001 (/usr/local/libexec/paperboat/users/u1000/bin/pb)"}
	var output bytes.Buffer
	if err := writeCLIJSON(&output, uninstallJSONResult("host", removed)); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data struct {
			Uninstalled   bool     `json:"uninstalled"`
			Hostname      string   `json:"hostname"`
			InboxKept     bool     `json:"inbox_preserved"`
			RemovedByName []string `json:"removed_by_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
		t.Fatalf("%v: %s", err, output.String())
	}
	got := envelope.Data
	if !got.Uninstalled || got.Hostname != "host" || !got.InboxKept || len(got.RemovedByName) != 2 || got.RemovedByName[0] != removed[0] || got.RemovedByName[1] != removed[1] {
		t.Fatalf("result = %+v (%s)", got, output.String())
	}
	output.Reset()
	if err := writeCLIJSON(&output, uninstallJSONResult("host", nil)); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(output.Bytes(), []byte("removed_by_name")) {
		t.Fatalf("an ordinary uninstall reports a by-name removal: %s", output.String())
	}
}
