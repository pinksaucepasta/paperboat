package config

import (
	"fmt"
	"time"
)

// Recordings are the Teams session recording limits the server hands to machines and enforces itself.
type Recordings struct {
	// MachineRetentionDays: a machine deletes a recording this many days after it started.
	MachineRetentionDays int `json:"machine_retention_days"`
	// DiskQuotaMB: the most disk a machine's recordings may use; the oldest are deleted first.
	DiskQuotaMB int `json:"disk_quota_mb"`
	// MaxKeyGenerations bounds rotations of one team's recording key (one row each).
	MaxKeyGenerations int64 `json:"max_key_generations"`
}

func defaultRecordings() Recordings {
	return Recordings{MachineRetentionDays: 30, DiskQuotaMB: 2048, MaxKeyGenerations: 100}
}

func (r Recordings) validate(index time.Duration) error {
	switch {
	case r.MachineRetentionDays < 1 || r.MachineRetentionDays > 3650, r.DiskQuotaMB < 256 || r.DiskQuotaMB > 65536,
		r.MaxKeyGenerations < 2 || r.MaxKeyGenerations > 10_000:
		return fmt.Errorf("recordings: machine_retention_days 1-3650, disk_quota_mb 256-65536, max_key_generations 2-10000")
	case index < time.Duration(r.MachineRetentionDays)*day:
		return fmt.Errorf("retention.recordings must be at least recordings.machine_retention_days")
	}
	return nil
}
