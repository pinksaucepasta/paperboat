package hostdproto

import (
	"context"
	"path/filepath"
	"strings"
)

const (
	TypeWorkerControlRequest  Type = "worker_control_request"
	TypeWorkerControlResponse Type = "worker_control_response"
)

// WorkerControlRequest selects only the fixed runtime worker entry point.
// Native owner validates the installed artifact slot; callers cannot supply
// commands, arguments, credentials or environment.
type WorkerControlRequest struct {
	Force      bool   `json:"force,omitempty"`
	Operation  string `json:"operation"`
	WorkerID   string `json:"worker_id,omitempty"`
	Executable string `json:"executable,omitempty"`
	Version    string `json:"version,omitempty"`
	APIMin     uint16 `json:"api_min,omitempty"`
	APIMax     uint16 `json:"api_max,omitempty"`
}

func (WorkerControlRequest) messageType() Type { return TypeWorkerControlRequest }
func (r WorkerControlRequest) validate() error {
	if r.Force && r.Operation != "prepare_maintenance" {
		return ErrInvalidFrame
	}
	switch r.Operation {
	case "start":
		if !validWorkerID(r.WorkerID) || !filepath.IsAbs(r.Executable) || filepath.Clean(r.Executable) != r.Executable || len(r.Executable) > 4096 || strings.ContainsAny(r.Executable, "\x00\r\n") || !validVersion(r.Version) || !validRange(r.APIMin, r.APIMax) {
			return ErrInvalidFrame
		}
	case "ready", "activate", "stop":
		if !validWorkerID(r.WorkerID) || r.Executable != "" || r.Version != "" || r.APIMin != 0 || r.APIMax != 0 {
			return ErrInvalidFrame
		}
	case "stop_active", "prepare_maintenance", "abort_maintenance":
		if r.WorkerID != "" || r.Executable != "" || r.Version != "" || r.APIMin != 0 || r.APIMax != 0 {
			return ErrInvalidFrame
		}
	default:
		return ErrInvalidFrame
	}
	return nil
}

type WorkerControlResponse struct {
	Status    *Status `json:"status,omitempty"`
	Completed bool    `json:"completed"`
}

func (WorkerControlResponse) messageType() Type { return TypeWorkerControlResponse }
func (r WorkerControlResponse) validate() error {
	if !r.Completed {
		return ErrInvalidFrame
	}
	if r.Status != nil {
		return r.Status.validate()
	}
	return nil
}

type WorkerControlHandler interface {
	HandleWorkerControl(context.Context, WorkerControlRequest) (WorkerControlResponse, error)
}
type WorkerControlClient interface {
	Request(context.Context, Message) (Message, error)
}

func ControlWorker(ctx context.Context, client WorkerControlClient, r WorkerControlRequest) (WorkerControlResponse, error) {
	response, err := client.Request(ctx, r)
	if err != nil {
		return WorkerControlResponse{}, err
	}
	v, ok := response.(*WorkerControlResponse)
	if !ok || v.validate() != nil {
		return WorkerControlResponse{}, ErrInvalidFrame
	}
	return *v, nil
}
