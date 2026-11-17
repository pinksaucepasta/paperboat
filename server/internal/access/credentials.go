// Package access defines the helper credential boundary used by enrolled machines.
package access

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/terminalprotocol"
	"time"
)

var (
	ErrTunnelUnavailable          = errors.New("access route is unavailable")
	ErrTerminalSessionNotFound    = errors.New("terminal session not found")
	ErrTerminalRuntimeUnavailable = errors.New("terminal runtime is unavailable")
)

type CredentialInput struct {
	UserID             string
	MachineID          string
	EnvironmentID      string
	SourceMachineID    string
	CLIClientSessionID string
	HTTPBaseURL        string
	ExpiresAt          time.Time
	AccessSessionID    string
}

type CredentialRevocationInput struct {
	UserID             string
	MachineID          string
	EnvironmentID      string
	CLIClientSessionID string
	HTTPBaseURL        string
	SessionIDs         []string
	Reason             string
}

type CLICredentials struct {
	TerminalAuth      *terminalprotocol.Auth
	FileTransferAuth  *terminalprotocol.Auth
	TerminalSessionID string
	FileSessionID     string
}

type CredentialIssuer interface {
	CheckCLI(context.Context, CredentialInput) error
	IssueCLI(context.Context, CredentialInput) (CLICredentials, error)
	RevokeCLI(context.Context, CredentialRevocationInput) error
}
