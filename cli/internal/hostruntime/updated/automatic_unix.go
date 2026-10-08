//go:build darwin || linux

package updated

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
)

func (s *Service) automaticCheck(ctx context.Context) (autoupdate.Result, error) {
	if s.config.Active.LocalSource != nil && s.config.Active.LocalSource.Distribution == installsource.Custom {
		return autoupdate.Result{Version: s.currentManager().ActiveVersion()}, nil
	}
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, maxUpdateControlTimeout)
	defer cancel()
	settings, err := machineUpdateSettings(s.config.StateRoot, s.config.AutomaticUpdates, nil)
	if err != nil {
		return autoupdate.Result{}, err
	}
	if !settings.Enabled {
		lock, err := unixActivationLock(s.config.StateRoot)
		if err != nil {
			return autoupdate.Result{}, err
		}
		defer lock.Close()
		result, err := resolveRelease(ctx, s.currentManager().ActiveVersion(), s.source.Resolve)
		return autoupdate.Result{Version: result.Version}, err
	}
	statePath := filepath.Join(s.config.StateRoot, "maintenance.json")
	state, err := autoupdate.LoadMaintenanceState(statePath)
	if err != nil {
		return autoupdate.Result{}, err
	}
	now := time.Now()
	due := settings.MaintenanceDue(now, state)
	// Download ahead of the window, retaining normal cohort eligibility. The
	// same exact verified candidate is reused by preparation and activation.
	candidate, err := s.downloadWithResolver(ctx, s.source.Resolve)
	if errors.Is(err, ErrActivationPending) {
		return autoupdate.Result{Version: s.currentManager().ActiveVersion()}, nil
	}
	if err != nil {
		return autoupdate.Result{}, err
	}
	result := autoupdate.Result{Version: s.currentManager().ActiveVersion()}
	if candidate.ID != "" {
		result.Version = candidate.Version
	}
	if !due {
		return result, nil
	}
	if candidate.ID == "" {
		return result, autoupdate.SaveMaintenanceState(statePath, settings, now, false)
	}
	if err := autoupdate.SaveMaintenanceState(statePath, settings, now, true); err != nil {
		return result, err
	}
	installed, err := s.queueActivationWithApproval(ctx, candidate.ID, false)
	if err != nil {
		return result, err
	}
	result.Version, result.Updated = installed.Version, installed.Updated
	return result, autoupdate.SaveMaintenanceState(statePath, settings, now, false)
}
