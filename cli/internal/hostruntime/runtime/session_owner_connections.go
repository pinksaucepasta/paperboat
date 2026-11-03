//go:build darwin || linux || windows

package runtime

import (
	"context"
	"path/filepath"
	"time"

	runtimeconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
)

func productionSessionOwners(ctx context.Context, cfg runtimeconfig.Config, environ func(string) string) (*sessionowner.Connections, error) {
	resources := cfg.Resources
	if resources == (runtimeconfig.ResourceLimits{}) {
		resources = runtimeconfig.DefaultResources
	}
	catalogRoot := environ("PAPERBOAT_SESSION_OWNER_CATALOG")
	_, source, sourceErr := installsource.Current()
	if sourceErr != nil {
		return nil, sourceErr
	}
	active, err := sessionowner.Read(filepath.Join(catalogRoot, source.SHA256[:32]+".json"))
	if err != nil {
		return nil, err
	}
	if active.RuntimeRoot != cfg.StateRoot || active.Source.SHA256 != source.SHA256 || active.Source.Length != source.Length {
		return nil, sessionowner.ErrInvalid
	}
	if err := validateIndependentAccount(active); err != nil {
		return nil, err
	}
	updater, err := updated.NewClient(environ("PAPERBOAT_UPDATED_SOCKET"), 30*time.Second)
	if err != nil {
		return nil, err
	}
	catalog := sessionowner.Catalog{Root: catalogRoot, ConfigRoot: filepath.Dir(catalogRoot), User: "enrolled", Group: "enrolled", OwnerSID: active.OwnerSID, Maximum: resources.MaxSessions + 1}
	return sessionowner.NewConnections(ctx, catalog, active, updater.StartSessionOwner, resources.MaxSessions)
}
