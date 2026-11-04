package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
)

// The process handler is the privacy boundary: a real authored source address
// permits only the bounded path enum, never arbitrary attributes or messages.
func TestCarrierMovePathSurvivesProcessPrivacyFilter(t *testing.T) {
	owner := reflect.ValueOf((*native.Session).DialCarrier).Pointer()
	foreign := reflect.ValueOf(TestCarrierMovePathSurvivesProcessPrivacyFilter).Pointer()
	for _, test := range []struct {
		name, path, want string
		source           uintptr
	}{
		{"punched", "direct-punched", "direct-punched", owner},
		{"peer relay", "peer_relay", "peer_relay", owner},
		{"regional relay", "regional_relay", "regional_relay", owner},
		{"unknown attribute", "PRIVATE_PATH_VALUE", "", owner},
		{"untrusted source", "direct-punched", "", foreign},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := diagnostics.NewMemoryRecorder()
			handler := processDiagnosticHandler{recorder: recorder, component: "pb"}
			record := slog.NewRecord(time.Now(), slog.LevelInfo, "terminal carrier move", test.source)
			record.Add("stage", "connected", "path", test.path, "private_payload", "PRIVATE_PAYLOAD_VALUE")
			if err := handler.Handle(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			events := recorder.Recent()
			if len(events) != 1 {
				t.Fatalf("retained events: %d", len(events))
			}
			if got := events[0].Fields["path_category"]; got != test.want {
				t.Fatalf("carrier path=%q, want %q", got, test.want)
			}
			body, err := json.Marshal(events)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "PRIVATE_") {
				t.Fatal("privacy filter retained an unapproved value")
			}
		})
	}
}
