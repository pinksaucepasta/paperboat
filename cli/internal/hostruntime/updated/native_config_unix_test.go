//go:build darwin || linux

package updated

import (
	"context"
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

// Status, selection, and admission fixtures have no native mutation authority.
// An unexpected activation fails instead of restoring the retired worker engine.
func nativeFixtureConfig(config Config) Config {
	if config.ApplyPrepared == nil {
		config.ApplyPrepared = func(context.Context, workerupdate.PreparedActivation) (installsource.Source, error) {
			return installsource.Source{}, errors.New("native activation is outside this fixture's contract")
		}
	}
	if config.LoadInstalledSource == nil {
		config.LoadInstalledSource = func() (installsource.Source, error) {
			if config.Active.LocalSource != nil {
				return *config.Active.LocalSource, nil
			}
			source := installsource.Source{Version: config.Active.Version, Platform: config.Active.Platform, Architecture: config.Active.Architecture, SHA256: config.Active.SHA256, Length: config.Active.Length, Distribution: installsource.Official, AutomaticUpdates: config.AutomaticUpdates}
			return source, source.Validate()
		}
	}
	return config
}
