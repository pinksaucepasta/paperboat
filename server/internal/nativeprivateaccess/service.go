package nativeprivateaccess

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
)

var ErrDenied = errors.New("native private access denied")
var ErrInvalid = errors.New("native private access request invalid")

type Target struct {
	CLIClientSessionID string `json:"cli_client_session_id,omitempty"`

	InstallationGeneration int64  `json:"installation_generation,omitempty"`
	BootID                 string `json:"boot_id,omitempty"`
	PolicyGeneration       int64  `json:"policy_generation,omitempty"`
	AnnouncementGeneration int64  `json:"announcement_generation,omitempty"`
	AccountID              string `json:"account_id"`
	UserID                 string `json:"user_id"`
	EnvironmentID          string `json:"environment_id"`
	MachineID              string `json:"machine_id"`
	AccessSessionID        string `json:"access_session_id"`
	ResourceKind           string `json:"resource_kind"`
	ResourceID             string `json:"resource_id"`
	ResourceGeneration     int64  `json:"resource_generation"`
	RouteID                string `json:"route_id"`
	RouteGeneration        int64  `json:"route_generation"`
	TargetGeneration       int64  `json:"target_generation"`
	Protocol               string `json:"protocol"`
	TargetScheme           string `json:"target_scheme"`
	TargetAddress          string `json:"target_address"`
}
type Request struct{ AccountID, UserID, CLIClientSessionID, OperationID, ResourceKind, ResourceID, RouteID, Protocol string }
type Resolver interface {
	ResolveNativePrivate(context.Context, Request) (Target, error)
}
type Signer interface {
	SignCredential(mint.CredentialInput) (string, error)
}
type Service struct {
	Resolver Resolver
	Signer   Signer
	Issuer   string
	Now      func() time.Time
}

type Grant struct {
	Target     Target    `json:"target"`
	Credential string    `json:"credential"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func (s Service) Issue(ctx context.Context, request Request) (Grant, error) {
	if ctx == nil || s.Resolver == nil || s.Signer == nil || s.Issuer == "" || request.OperationID == "" || request.ResourceKind != "machine_service" || request.Protocol != "tcp" {
		return Grant{}, ErrInvalid
	}
	target, err := s.Resolver.ResolveNativePrivate(ctx, request)
	if err != nil {
		if pureNativeRejection(err, ErrDenied) {
			return Grant{}, ErrDenied
		}
		return Grant{}, err
	}
	if !matchesRequest(target, request) || !validTarget(target) {
		return Grant{}, ErrDenied
	}
	target.CLIClientSessionID = request.CLIClientSessionID
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	// JWT NumericDate is second-granular. Return the identical instant in the
	// stream binding so host comparison cannot reject a valid grant because the
	// JSON timestamp retained sub-second precision.
	expires := now.Add(5 * time.Minute).Truncate(time.Second)
	credential, err := s.Signer.SignCredential(mint.CredentialInput{Issuer: s.Issuer, Audience: "paperboat-machine", Subject: target.UserID, JTI: "token_" + uuid.NewString(), IssuedAt: now, ExpiresAt: expires, CredentialClass: "native_private", Scopes: []string{"private:native"}, EnvironmentID: target.EnvironmentID, AccountID: target.AccountID, MachineID: target.MachineID, UserID: target.UserID, CLIClientSessionID: request.CLIClientSessionID, AssignmentID: target.AccessSessionID, OperationID: request.OperationID, ResourceKind: target.ResourceKind, ResourceID: target.ResourceID, RouteID: target.RouteID, Protocol: target.Protocol, TargetScheme: target.TargetScheme, TargetAddress: target.TargetAddress, ExpectedGeneration: target.ResourceGeneration, RouteGeneration: target.RouteGeneration, TargetGeneration: target.TargetGeneration, InstallationGeneration: target.InstallationGeneration, BootID: target.BootID, PolicyGeneration: target.PolicyGeneration, AnnouncementGeneration: target.AnnouncementGeneration})
	if err != nil {
		return Grant{}, err
	}
	return Grant{Target: target, Credential: credential, ExpiresAt: expires}, nil
}

func (s Service) Revalidate(ctx context.Context, request Request, expected Target) error {
	current, err := s.Resolver.ResolveNativePrivate(ctx, request)
	if err != nil {
		if pureNativeRejection(err, ErrDenied) {
			return ErrDenied
		}
		return err
	}
	if current != expected || !matchesRequest(current, request) || !validTarget(current) {
		return ErrDenied
	}
	return nil
}

// A credential or resource rejection is expected only when its complete,
// bounded unwrap tree contains no independent operational cause.
func pureNativeRejection(err, expected error) (rejected bool) {
	defer func() {
		if recover() != nil {
			rejected = false
		}
	}()
	if err == nil || expected == nil {
		return false
	}
	nodes, matched := 0, false
	var visit func(error, int) bool
	visit = func(current error, depth int) bool {
		if current == nil || depth >= 8 || nodes >= 32 {
			return false
		}
		typ := reflect.TypeOf(current)
		value := reflect.ValueOf(current)
		if typ == nil || (value.Kind() == reflect.Ptr || value.Kind() == reflect.Interface || value.Kind() == reflect.Map || value.Kind() == reflect.Slice || value.Kind() == reflect.Func) && value.IsNil() {
			return false
		}
		nodes++
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return false
			}
			for _, child := range children {
				if !visit(child, depth+1) {
					return false
				}
			}
			return true
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			return visit(wrapped.Unwrap(), depth+1)
		}
		if !typ.Comparable() || typ != reflect.TypeOf(expected) || current != expected {
			return false
		}
		matched = true
		return true
	}
	return visit(err, 0) && matched
}

func matchesRequest(t Target, r Request) bool {
	return t.AccountID == r.AccountID && t.UserID == r.UserID && t.ResourceKind == r.ResourceKind && t.ResourceID == r.ResourceID && t.RouteID == r.RouteID && t.Protocol == r.Protocol
}
func validTarget(t Target) bool {
	host, port, err := net.SplitHostPort(t.TargetAddress)
	n, pe := strconv.ParseUint(port, 10, 16)
	scheme := t.Protocol == "tcp" && t.TargetScheme == "tcp"
	return t.AccountID != "" && t.UserID != "" && t.EnvironmentID != "" && t.MachineID != "" && t.AccessSessionID != "" && (t.ResourceKind == "machine_service" && t.ResourceID == t.MachineID && t.RouteID == "tcp:"+port && t.Protocol == "tcp" && t.InstallationGeneration > 0 && t.BootID != "" && t.PolicyGeneration > 0 && t.AnnouncementGeneration > 0) && t.ResourceID != "" && t.RouteID != "" && t.ResourceGeneration > 0 && t.RouteGeneration > 0 && t.TargetGeneration > 0 && scheme && err == nil && pe == nil && n > 0 && (host == "127.0.0.1" || host == "::1")
}
