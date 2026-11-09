package server

import (
	"encoding/json"
	"runtime"
	"testing"
)

func TestDashboardTerminalCreationPayloadIsAccepted(t *testing.T) {
	root := "/home/anvit"
	if runtime.GOOS == "windows" {
		root = `C:\Users\anvit` // an absolute path there has a drive
	}
	payload, err := json.Marshal(map[string]any{"action": "create", "session_id": "umts_example", "name": "probe-a", "cwd": root, "columns": 80, "rows": 24, "existing_snapshot": true})
	if err != nil {
		t.Fatal(err)
	}
	var request terminalRequest
	if err := decodeStrict(payload, &request); err != nil {
		t.Fatal(err)
	}
	dispatcher := Dispatcher{config: DispatcherConfig{WorkspaceRoot: root}}
	if cwd, ok := dispatcher.cwd(request.CWD); !ok || cwd != root || request.Columns != 80 || request.Rows != 24 || !request.ExistingSnapshot {
		t.Fatalf("invalid creation request: cwd=%q valid=%v columns=%d rows=%d", cwd, ok, request.Columns, request.Rows)
	}
}
