//go:build windows

package main

import (
	"context"

	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
)

func platformDoctorRepair(ctx context.Context) error {
	return repairEnrolled(ctx, hostruntimecmd.EnrolledInstallation, hostruntimecmd.RepairManagedHost)
}
