package db

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

var sessionSettingNames = []string{"search_path", "jit", "statement_timeout", "lock_timeout", "idle_in_transaction_session_timeout"}

// Default: connections start with the session settings (search_path, jit, the timeouts) as startup parameters.
func TestStartupParamsAreSessionSettingsByDefault(t *testing.T) {
	store, err := Open(config.Database{Driver: "postgres", DSN: "postgres://app:secret@db.example.test:5432/paperboat"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	params := store.Pool().Config().ConnConfig.RuntimeParams
	want := map[string]string{"search_path": "paperboat", "jit": "off", "statement_timeout": "30000", "lock_timeout": "10000", "idle_in_transaction_session_timeout": "60000"}
	for name, value := range want {
		if params[name] != value {
			t.Errorf("startup parameter %s = %q, want %q", name, params[name], value)
		}
	}
}

// With startup_params false (behind a pooler that rejects them) no pool sends them: the role defaults provide them.
func TestStartupParamsOffSendsNoSessionSettings(t *testing.T) {
	off := false
	store, err := Open(config.Database{Driver: "postgres", DSN: "postgres://app:secret@pooler.example.test:6432/paperboat", DirectDSN: "postgres://app:secret@db.example.test:5432/paperboat", StartupParams: &off})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, pool := range []struct {
		name   string
		params map[string]string
	}{{"pooled", store.Pool().Config().ConnConfig.RuntimeParams}, {"direct", store.Direct().Config().ConnConfig.RuntimeParams}, {"migration", store.migrationConfig.ConnConfig.RuntimeParams}} {
		for _, name := range sessionSettingNames {
			if value, ok := pool.params[name]; ok {
				t.Errorf("%s pool sends startup parameter %s=%q with startup_params false", pool.name, name, value)
			}
		}
	}
}
