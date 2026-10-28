package usermachines

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// The dashboard counted machines from the first 200-row page of the list, so an account with more machines had
// wrong totals, and revoked machines showed as offline. The summary counts the whole workspace; removed machines
// are in no count and no list.
func TestMachineSummaryCountsTheWholeWorkspaceOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	// live_user_machines is SELECT *: a column added to user_machines later must be added to the view too.
	var missing int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns t WHERE t.table_schema='paperboat' AND t.table_name='user_machines'
 AND NOT EXISTS(SELECT 1 FROM information_schema.columns v WHERE v.table_schema='paperboat' AND v.table_name='live_user_machines' AND v.column_name=t.column_name)`).Scan(&missing); err != nil || missing != 0 {
		t.Fatalf("live_user_machines lacks %d user_machines columns, err=%v", missing, err)
	}
	user := fmt.Sprintf("ms_user_%d", time.Now().UnixNano())
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user); err != nil {
		t.Fatal(err)
	}
	// 230 machines in use (more than one 200-row page): 120 linux online, 70 darwin offline, 30 windows online,
	// 10 linux pending; and 12 revoked plus 8 disconnected, which are removed and counted nowhere.
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,revoked_at)
SELECT $1||'_'||g,$1,$1||'_'||g,'ms-'||g,
 CASE WHEN g<120 OR g BETWEEN 220 AND 229 OR g>=230 THEN 'linux' WHEN g<190 THEN 'darwin' ELSE 'windows' END,'amd64','/w',
 CASE WHEN g<120 OR g BETWEEN 190 AND 219 THEN 'online' WHEN g<190 THEN 'offline' WHEN g<230 THEN 'pending' WHEN g<242 THEN 'revoked' ELSE 'disconnected' END,
 'occupied',g<120 OR g BETWEEN 190 AND 219,CASE WHEN g>=230 THEN now() END
FROM generate_series(0,249) g`, user); err != nil {
		t.Fatal(err)
	}
	service := New(store, nil, Policy{}, nil)
	personal := workspace.WithContext(ctx, workspace.Scope{ID: "personal"})
	summary, err := service.Summary(personal, user)
	if err != nil {
		t.Fatal(err)
	}
	want := MachineSummary{Total: 230,
		ByState:    map[string]int{"online": 150, "offline": 70, "pending": 10},
		ByPlatform: map[string]int{"linux": 130, "macos": 70, "windows": 30}}
	if fmt.Sprint(summary) != fmt.Sprint(want) {
		t.Fatalf("summary %+v, want %+v", summary, want)
	}
	items, total, _, err := service.ListInventory(personal, user, InventoryOptions{Limit: 200})
	if err != nil || len(items) != 200 || total != 230 {
		t.Fatalf("default list: %d items, total %d, %v; want a 200-row page of 230 machines in use", len(items), total, err)
	}
	for _, item := range items {
		if item.State == "revoked" || item.State == "disconnected" {
			t.Fatalf("the default list shows %s machine %s", item.State, item.ID)
		}
	}
	// Removed machines cannot be asked for either.
	for _, state := range []string{"revoked", "disconnected"} {
		if _, total, _, err := service.ListInventory(personal, user, InventoryOptions{Limit: 50, State: state}); err != nil || total != 0 {
			t.Fatalf("state=%s: total %d, %v; want no removed machines", state, total, err)
		}
	}
}
