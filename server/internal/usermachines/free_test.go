package usermachines

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"testing"
	"time"
)

func TestFreeAccountSetupWithoutSubscription(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	user := "free_probe_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user); err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{AllowedPlatforms: []string{"linux"}}, testSeatAuthorizer{})
	service.ConfigureAccess(nil, "https://api.example.test", 5*time.Minute)
	if err := service.ConfigureRuntimeRoute("runtime.example.test", 38080); err != nil {
		t.Fatal(err)
	}
	_, err := service.Setup(ctx, user, SetupInput{Alias: "free", Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/home/paperboat", PublicIdentityKey: newIntegrationPublicIdentityKey(t), RuntimeVersions: json.RawMessage(`{"pb":"test"}`)})
	if err != nil {
		t.Fatal(err)
	}
}
