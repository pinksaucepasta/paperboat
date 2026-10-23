package usermachines

import (
	"database/sql"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/helperruntime"
)

func TestTerminalIdentificationProjectsLiveSanitizedMetadata(t *testing.T) {
	item := SharedTerminalSession{Name: "my-work", StartedIn: "/launch"}
	projectTerminalIdentification(&item, helperruntime.Snapshot{Title: "  vim\x1b\n\u202e file ", CurrentDirectory: "/live\x00/\u2066project", ForegroundProcess: "vi\t\u200fm", CWD: "/different-launch"})
	if item.Name != "my-work" || item.StartedIn != "/launch" || item.Title != "vim file" || item.CurrentDirectory != "/live/project" || item.ForegroundProcess != "vim" {
		t.Fatalf("incorrect live projection: %+v", item)
	}
	projectTerminalIdentification(&item, helperruntime.Snapshot{Title: strings.Repeat("界", 129), CurrentDirectory: strings.Repeat("界", 1025), ForegroundProcess: strings.Repeat("界", 129)})
	if utf8.RuneCountInString(item.Title) != 128 || utf8.RuneCountInString(item.CurrentDirectory) != 1024 || utf8.RuneCountInString(item.ForegroundProcess) != 128 {
		t.Fatal("metadata exceeds display bounds")
	}
	projectTerminalIdentification(&item, helperruntime.Snapshot{CWD: "/launch"})
	if item.Title != "" || item.CurrentDirectory != "" || item.ForegroundProcess != "" {
		t.Fatalf("missing observation retained stale metadata: %+v", item)
	}
}

func TestTerminalIdentificationNeverUsesStoredManualTitleOrLaunchAsCurrent(t *testing.T) {
	item := mapTerminalSession(dbsqlc.UserMachineTerminalSession{Name: "work", Title: sql.NullString{String: "stale manual title", Valid: true}, LaunchCwd: "/launch"})
	if item.Title != "" || item.CurrentDirectory != "" || item.ForegroundProcess != "" || item.StartedIn != "/launch" {
		t.Fatalf("durable metadata misrepresented as live: %+v", item)
	}
}
