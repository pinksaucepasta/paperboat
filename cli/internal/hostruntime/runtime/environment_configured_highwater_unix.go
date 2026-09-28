//go:build darwin || linux || windows

package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

const configuredEnvironmentHighWaterSchema = "paperboat.environment-configured-high-water/v1"

type configuredEnvironmentHighWater struct {
	Schema                 string `json:"schema"`
	Issuer                 string `json:"issuer"`
	MachineID              string `json:"machine_id"`
	InstallationGeneration uint64 `json:"installation_generation"`
}

func (s *projectionEnvironmentService) configuredHighWaterPath() string {
	return filepath.Join(s.stateRoot, "environment", "configured-high-water.json")
}

func (s *projectionEnvironmentService) configuredHighWater() configuredEnvironmentHighWater {
	return configuredEnvironmentHighWater{
		Schema:                 configuredEnvironmentHighWaterSchema,
		Issuer:                 strings.TrimRight(s.base.String(), "/"),
		MachineID:              s.registration.MachineID,
		InstallationGeneration: uint64(s.registration.InstallationGeneration),
	}
}

func (s *projectionEnvironmentService) setConfiguredSeen() {
	s.mu.Lock()
	s.configuredSeen = true
	s.mu.Unlock()
}

// restoreConfiguredHighWater treats any present marker as configured evidence,
// even when its contents are damaged. Invalid state therefore fails closed.
func (s *projectionEnvironmentService) restoreConfiguredHighWater() (bool, error) {
	path := s.configuredHighWaterPath()
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		s.setConfiguredSeen()
		return true, err
	}
	s.setConfiguredSeen()
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || info.Size() > 4096 {
		return true, errors.New("invalid configured ENV high-water file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return true, err
	}
	want, err := json.Marshal(s.configuredHighWater())
	if err != nil || !bytes.Equal(raw, want) {
		return true, errors.New("configured ENV high-water identity mismatch")
	}
	return true, nil
}

func (s *projectionEnvironmentService) markConfiguredHighWater() error {
	// Set in memory before disk I/O so concurrent launch checks cannot downgrade
	// a configured response if persistence fails.
	s.setConfiguredSeen()
	if s.stateRoot == "" || s.registration.MachineID == "" || s.registration.InstallationGeneration < 1 {
		return errors.New("invalid configured ENV high-water identity")
	}
	raw, err := json.Marshal(s.configuredHighWater())
	if err != nil {
		return err
	}
	path := s.configuredHighWaterPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return atomicfile.Write(path, raw, atomicfile.CurrentOwnerOptions(0o600))
}

func exactEnvironmentUnconfigured(raw []byte, registration identity.Registration, hostKeyGeneration uint64, hostPublic string) bool {
	var envelope struct {
		Error *struct {
			Code    string `json:"code"`
			Details *struct {
				MachineID               string  `json:"machine_id"`
				InstallationGeneration  *uint64 `json:"installation_generation"`
				HostKeyGeneration       *uint64 `json:"host_key_generation"`
				HostPublic              string  `json:"host_public"`
				AccountConfigGeneration *uint64 `json:"account_config_generation"`
			} `json:"details"`
		} `json:"error"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if decoder.Decode(&envelope) != nil || decoder.Decode(&struct{}{}) != io.EOF || envelope.Error == nil || envelope.Error.Details == nil {
		return false
	}
	details := envelope.Error.Details
	return envelope.Error.Code == "environment_unconfigured" &&
		details.MachineID == registration.MachineID &&
		details.InstallationGeneration != nil && *details.InstallationGeneration == uint64(registration.InstallationGeneration) &&
		details.HostKeyGeneration != nil && *details.HostKeyGeneration == hostKeyGeneration &&
		details.HostPublic == hostPublic &&
		details.AccountConfigGeneration != nil && *details.AccountConfigGeneration == 0 &&
		registration.MachineID != "" && registration.InstallationGeneration > 0 && hostKeyGeneration > 0 && hostPublic != ""
}
