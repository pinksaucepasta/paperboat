package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/auth"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/route"
	edgeruntime "github.com/pinksaucepasta/paperboat-tunnel/internal/runtime"
	"strings"
)

func serviceDiagnostic(err error) string {
	components := []string{}
	var visit func(error)
	visit = func(e error) {
		if e == nil {
			return
		}
		if stage, ok := e.(*edgeruntime.StartError); ok {
			components = append(components, stage.Component)
		}
		switch x := e.(type) {
		case interface{ Unwrap() []error }:
			for _, child := range x.Unwrap() {
				visit(child)
			}
		case interface{ Unwrap() error }:
			visit(x.Unwrap())
		}
	}
	visit(err)
	category := "internal"
	switch {
	case errors.Is(err, auth.ErrSnapshotInvalid):
		category = "trust_snapshot_invalid"
	case errors.Is(err, control.ErrNodeObservationStale):
		category = "node_observation_stale"
	case errors.Is(err, control.ErrControlUnavailable):
		category = "control_unavailable"
	case errors.Is(err, control.ErrControlInvalid):
		category = "control_invalid"
	case errors.Is(err, route.ErrInvalid):
		category = "route_invalid"
	case errors.Is(err, context.DeadlineExceeded):
		category = "deadline"
	case errors.Is(err, context.Canceled):
		category = "canceled"
	}
	detail := fmt.Sprintf("service failure component=%s category=%s", strings.Join(components, "/"), category)
	var request *control.RequestFailure
	if errors.As(err, &request) {
		detail += fmt.Sprintf(" control_path=%s http_status=%d control_failure=%s", request.Path, request.Status, request.Category)
	}
	return detail
}
