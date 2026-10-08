package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

var ErrCredentialPolicy = errors.New("credential policy unavailable")

type CredentialVerifier interface {
	Verify(context.Context, string, auth.Policy) (auth.Claims, error)
}

type CredentialRevocationWatcher interface {
	Watch(string) (*atomic.Bool, <-chan struct{}, func(), error)
}

type PolicyResolver interface {
	Policy(protocol.Frame) (auth.Policy, error)
}

type CredentialAuthorizer struct {
	Verifier      CredentialVerifier
	Resolver      PolicyResolver
	Token         string
	Revocations   CredentialRevocationWatcher
	mu            sync.Mutex
	jti           string
	revoked       *atomic.Bool
	revokedSignal <-chan struct{}
	release       func()
}

func (a *CredentialAuthorizer) Authorize(ctx context.Context, frame protocol.Frame) (Authorization, error) {
	if a.Verifier == nil || a.Resolver == nil || a.Token == "" {
		return Authorization{}, ErrCredentialPolicy
	}
	policy, err := a.Resolver.Policy(frame)
	if err != nil {
		return Authorization{}, err
	}
	claims, err := a.Verifier.Verify(ctx, a.Token, policy)
	if err != nil {
		return Authorization{}, err
	}
	if frame.Capability == "file-transfer.v1" && claims.SourceMachineID == "" && claims.CredentialClass != "browser_terminal_operation" {
		return Authorization{}, ErrCredentialPolicy
	}
	role := terminalRole(claims)
	if claims.CredentialClass == "terminal_operation" && (role == "" || claims.SessionID == "" || claims.CLIClientSessionID == "" || role != TerminalRoleOwner && (claims.AccountID == "" || claims.ExpectedGeneration < 1)) {
		return Authorization{}, ErrCredentialPolicy
	}
	browserTerminal := claims.CredentialClass == "browser_terminal_operation"
	terminalFrame := frame.Capability == "terminal.v1" || frame.Type == "ack" || frame.Type == "detach"
	browserUpload := frame.Capability == "file-transfer.v1" && role == TerminalRoleOwner
	if browserTerminal && ((!terminalFrame && !browserUpload) || !validBrowserTerminalClaims(claims)) {
		return Authorization{}, ErrCredentialPolicy
	}
	if claims.CredentialClass == "config_compare" || claims.CredentialClass == "browser_config_compare" {
		if frame.Capability != "config.compare.v1" || !validConfigCompareClaims(claims) {
			return Authorization{}, ErrCredentialPolicy
		}
	}
	var revoked *atomic.Bool
	if a.Revocations != nil {
		a.mu.Lock()
		if a.jti != "" && a.jti != claims.JTI {
			a.mu.Unlock()
			return Authorization{}, ErrCredentialPolicy
		}
		if a.revoked == nil {
			a.revoked, a.revokedSignal, a.release, err = a.Revocations.Watch(claims.JTI)
			a.jti = claims.JTI
		}
		revoked = a.revoked
		a.mu.Unlock()
		if err != nil {
			return Authorization{}, err
		}
	}
	binding, err := stableClaimsBinding(claims)
	if err != nil {
		return Authorization{}, err
	}
	resourceID := claims.AssignmentID
	accountID := claims.AccountID
	if accountID == "" && role == TerminalRoleOwner {
		accountID = claims.UserID
	}
	clientID := claims.CLIClientSessionID
	if browserTerminal || claims.CredentialClass == "browser_config_compare" {
		clientID = claims.BrowserAttachmentID
	}
	if (claims.CredentialClass == "codex_manage" || claims.CredentialClass == "codex_connect") && resourceID == "" {
		resourceID = claims.SessionID
	}
	return Authorization{
		JournalBinding:      binding,
		EnvironmentID:       claims.EnvironmentID,
		MachineID:           claims.MachineID,
		SourceMachineID:     claims.SourceMachineID,
		UserID:              claims.UserID,
		AccountID:           accountID,
		ClientID:            clientID,
		SessionID:           claims.SessionID,
		TerminalRole:        role,
		TerminalGeneration:  uint64(claims.ExpectedGeneration),
		BrowserTerminal:     browserTerminal,
		BrowserAttachmentID: claims.BrowserAttachmentID,
		PolicyGeneration:    uint64(claims.PolicyGeneration),
		ResourceID:          resourceID,
		OperationID:         claims.OperationID,
		RequestID:           claims.RequestID,
		RequestHash:         claims.RequestHash,
		IdempotencyKey:      claims.IdempotencyKey,
		ExpiresAt:           time.Unix(claims.ExpiresAt, 0).UTC(),
		Revoked:             revoked,
		RevokedSignal:       a.revokedSignal,
		Value:               claims,
	}, nil
}

// BrowserTerminalPublicKeySHA256 verifies the terminal credential before TLS
// starts and returns its browser client certificate pin. The credential is
// verified again when ServeAuthenticated authorizes application frames.
func (a *CredentialAuthorizer) BrowserTerminalPublicKeySHA256(ctx context.Context) (string, error) {
	if a.Verifier == nil || a.Resolver == nil || a.Token == "" {
		return "", ErrCredentialPolicy
	}
	policy, err := a.Resolver.Policy(protocol.Frame{Capability: "terminal.v1"})
	if err != nil {
		return "", err
	}
	claims, err := a.Verifier.Verify(ctx, a.Token, policy)
	if err != nil {
		return "", err
	}
	if claims.CredentialClass != "browser_terminal_operation" || !validBrowserTerminalClaims(claims) {
		return "", ErrCredentialPolicy
	}
	return claims.BrowserPublicKeySHA256, nil
}

func validBrowserTerminalClaims(claims auth.Claims) bool {
	return terminalRole(claims) != "" && claims.EnvironmentID != "" && claims.AccountID != "" && claims.UserID != "" && claims.MachineID != "" && claims.SessionID != "" && claims.BrowserAttachmentID != "" && claims.ExpectedGeneration > 0 && claims.PolicyGeneration > 0 && claims.BrowserPublicKeySHA256 != ""
}

func terminalRole(claims auth.Claims) TerminalRole {
	if claims.CredentialClass != "terminal_operation" && claims.CredentialClass != "browser_terminal_operation" || len(claims.Scope) != 1 {
		return ""
	}
	switch claims.Scope[0] {
	case "terminal:operate":
		return TerminalRoleOwner
	case "terminal:view":
		return TerminalRoleViewer
	case "terminal:control":
		return TerminalRoleInteractive
	default:
		return ""
	}
}

func (a *CredentialAuthorizer) CloseAuthorization() {
	a.mu.Lock()
	if a.release != nil {
		a.release()
		a.release = nil
	}
	a.mu.Unlock()
}

func stableClaimsBinding(claims auth.Claims) (string, error) {
	// Exclude token-instance fields (JTI and timestamps) so a renewed credential
	// can retrieve the same durable operation result without crossing identity,
	// resource, class, or exact-scope boundaries.
	encoded, err := json.Marshal(struct {
		Issuer                 string   `json:"issuer"`
		Subject                string   `json:"subject"`
		Class                  string   `json:"class"`
		Scopes                 []string `json:"scopes"`
		EnvironmentID          string   `json:"environment_id"`
		AccountID              string   `json:"account_id"`
		MachineID              string   `json:"machine_id"`
		SourceMachineID        string   `json:"source_machine_id"`
		UserID                 string   `json:"user_id"`
		ActorID                string   `json:"actor_id"`
		CLIClientSessionID     string   `json:"cli_client_session_id"`
		HelperID               string   `json:"helper_id"`
		SessionID              string   `json:"session_id"`
		BrowserAttachmentID    string   `json:"browser_attachment_id"`
		BrowserPublicKeySHA256 string   `json:"browser_public_key_sha256"`
		PolicyGeneration       int64    `json:"policy_generation"`
		OperationID            string   `json:"operation_id"`
		AssignmentID           string   `json:"assignment_id"`
		PreviewID              string   `json:"preview_id"`
		OwnerSessionID         string   `json:"owner_session_id"`
		OwnerSessionKind       string   `json:"owner_session_kind,omitempty"`
		ExpectedGeneration     int64    `json:"expected_generation"`
		IdempotencyKey         string   `json:"idempotency_key"`
		RequestID              string   `json:"request_id"`
		CorrelationID          string   `json:"correlation_id"`
		RequestHash            string   `json:"request_hash"`
	}{claims.Issuer, claims.Subject, claims.CredentialClass, claims.Scope, claims.EnvironmentID, claims.AccountID, claims.MachineID, claims.SourceMachineID, claims.UserID, claims.ActorID, claims.CLIClientSessionID, claims.HelperID, claims.SessionID, claims.BrowserAttachmentID, claims.BrowserPublicKeySHA256, claims.PolicyGeneration, claims.OperationID, claims.AssignmentID, claims.PreviewID, claims.OwnerSessionID, claims.OwnerSessionKind, claims.ExpectedGeneration, claims.IdempotencyKey, claims.RequestID, claims.CorrelationID, claims.RequestHash})
	if err != nil {
		return "", err
	}
	if claims.CredentialClass == "config_compare" || claims.CredentialClass == "browser_config_compare" {
		tuple, err := json.Marshal(struct {
			Binding           json.RawMessage `json:"binding"`
			AssignmentVersion int64           `json:"assignment_version"`
			Path              string          `json:"path"`
			Conflict          string          `json:"conflict"`
			Remote            string          `json:"remote"`
			Generation        int64           `json:"generation"`
		}{encoded, claims.AssignmentVersion, claims.ConfigPath, claims.ConflictRevision, claims.ExpectedRemoteRevision, claims.InstallationGeneration})
		if err != nil {
			return "", err
		}
		encoded = tuple
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func validConfigCompareClaims(c auth.Claims) bool {
	if c.AssignmentID == "" || c.AssignmentVersion < 1 || c.ConfigPath == "" || c.ConflictRevision == "" || c.ExpectedRemoteRevision == "" || c.EnvironmentID == "" || c.MachineID == "" || c.InstallationGeneration < 1 || c.UserID == "" || c.AccountID == "" {
		return false
	}
	if c.CredentialClass == "browser_config_compare" {
		return c.BrowserAttachmentID != "" && c.BrowserPublicKeySHA256 != "" && c.SourceMachineID == "" && c.CLIClientSessionID == ""
	}
	return c.CredentialClass == "config_compare" && c.SourceMachineID != "" && c.CLIClientSessionID != ""
}
func (a *CredentialAuthorizer) BrowserConfigComparePublicKeySHA256(ctx context.Context) (string, error) {
	if a.Verifier == nil || a.Resolver == nil || a.Token == "" {
		return "", ErrCredentialPolicy
	}
	policy, err := a.Resolver.Policy(protocol.Frame{Capability: "config.compare.v1"})
	if err != nil {
		return "", err
	}
	c, err := a.Verifier.Verify(ctx, a.Token, policy)
	if err != nil {
		return "", err
	}
	if c.CredentialClass != "browser_config_compare" || !validConfigCompareClaims(c) {
		return "", ErrCredentialPolicy
	}
	return c.BrowserPublicKeySHA256, nil
}
