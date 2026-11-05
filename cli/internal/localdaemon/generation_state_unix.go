//go:build darwin || linux

package localdaemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
)

// GenerationState is what one running daemon generation reports about itself for pb status and pb
// doctor. A draining generation has released the local API socket, so it reports through this file.
type GenerationState struct {
	Generation string    `json:"generation"`
	PID        int       `json:"pid"`
	State      string    `json:"state"` // "live" or "draining"
	Streams    int       `json:"streams"`
	DrainUntil time.Time `json:"drain_until,omitzero"`
}

func generationStatePath(stateRoot, generation string) string {
	return filepath.Join(stateRoot, "generations", generation+".json")
}

// WriteGenerationState publishes this process's generation state.
func WriteGenerationState(stateRoot string, state GenerationState) error {
	if !validGeneration(state.Generation) || !filepath.IsAbs(stateRoot) {
		return ErrInvalidInventoryConfig
	}
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	path := generationStatePath(stateRoot, state.Generation)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return atomicfile.Write(path, body, atomicfile.CurrentOwnerOptions(0o600))
}

// RemoveGenerationState withdraws a generation's state when its process exits.
func RemoveGenerationState(stateRoot, generation string) error {
	if !validGeneration(generation) {
		return ErrInvalidInventoryConfig
	}
	err := os.Remove(generationStatePath(stateRoot, generation))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// GenerationStates lists the running daemon generations, the live one first, then the draining ones
// by deadline. A state whose process is gone, or a drain past its deadline, is left out and removed.
func GenerationStates(stateRoot string, now time.Time) ([]GenerationState, error) {
	matches, err := filepath.Glob(filepath.Join(stateRoot, "generations", "*.json"))
	if err != nil {
		return nil, err
	}
	var states []GenerationState
	for _, match := range matches {
		body, err := os.ReadFile(match)
		if err != nil {
			continue
		}
		var state GenerationState
		if json.Unmarshal(body, &state) != nil || !validGeneration(state.Generation) || generationStatePath(stateRoot, state.Generation) != match {
			continue
		}
		if state.PID < 1 || syscall.Kill(state.PID, 0) == syscall.ESRCH || state.State == "draining" && !now.Before(state.DrainUntil) {
			_ = os.Remove(match)
			continue
		}
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool {
		if states[i].State != states[j].State {
			return states[i].State == "live"
		}
		return states[i].DrainUntil.Before(states[j].DrainUntil)
	})
	return states, nil
}
