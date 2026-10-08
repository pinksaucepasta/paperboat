package connectorprotocol

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestDefaultSessionIDsUseCanonicalConvention(t *testing.T) {
	server, err := NewServer(ServerConfig{Capabilities: requiredCapabilityList(), Authenticator: AuthenticatorFuncs{}, Snapshots: SnapshotSourceFunc(func(context.Context, string) (Snapshot, error) { return Snapshot{}, nil })})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for range 2 {
		id, err := server.config.SessionIDs()
		parsed, parseErr := uuid.Parse(strings.TrimPrefix(id, "session_"))
		if err != nil || parseErr != nil || parsed.Version() != 4 || parsed.Variant() != uuid.RFC4122 || "session_"+parsed.String() != id {
			t.Fatalf("session identity = %q, %v", id, err)
		}
		if seen[id] {
			t.Fatal("session identity reused")
		}
		seen[id] = true
	}
}
