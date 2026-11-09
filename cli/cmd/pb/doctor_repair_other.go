//go:build !windows

package main

import "context"

func platformDoctorRepair(context.Context) error { return nil }
