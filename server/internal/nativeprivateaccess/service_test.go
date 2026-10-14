package nativeprivateaccess

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/mint"
)

type resolverFunc func(context.Context, Request) (Target, error)

func (f resolverFunc) ResolveNativePrivate(c context.Context, r Request) (Target, error) {
	return f(c, r)
}

func TestNativeSQLTransactionCleanupPreservesCausesAndOnlyIgnoresExactDone(t *testing.T) {
	const privateCanary = "NATIVE_SQL_PRIVATE_CLEANUP_CANARY"
	operation := fmt.Errorf("operation %s: %w", privateCanary, ErrDenied)
	cleanup := syscall.EIO
	combined := preserveNativeSQLTransactionCleanup(operation, cleanup)
	if !errors.Is(combined, ErrDenied) || !errors.Is(combined, cleanup) {
		t.Fatalf("cleanup result lost a cause: %v", combined)
	}
	if combined.Error() != "native private access transaction cleanup failed" || strings.Contains(combined.Error(), privateCanary) {
		t.Fatalf("cleanup wrapper exposed arbitrary cause text: %q", combined.Error())
	}
	if got := preserveNativeSQLTransactionCleanup(operation, nil); got != operation {
		t.Fatalf("nil rollback changed operation result: %v", got)
	}
	if got := preserveNativeSQLTransactionCleanup(operation, sql.ErrTxDone); got != operation {
		t.Fatalf("exact completed-transaction sentinel was not suppressed: %v", got)
	}
	wrappedDone := fmt.Errorf("wrapped done: %w", sql.ErrTxDone)
	got := preserveNativeSQLTransactionCleanup(operation, wrappedDone)
	if got == operation || !errors.Is(got, ErrDenied) || !errors.Is(got, sql.ErrTxDone) {
		t.Fatalf("wrapped completion failure was suppressed or lost: %v", got)
	}
	uncomparable := nativeCleanupUncomparableError{detail: []byte(privateCanary)}
	got = preserveNativeSQLTransactionCleanup(operation, uncomparable)
	var observed nativeCleanupUncomparableError
	if !errors.Is(got, ErrDenied) || !errors.As(got, &observed) || string(observed.detail) != privateCanary || strings.Contains(got.Error(), privateCanary) {
		t.Fatalf("uncomparable cleanup cause was not retained safely: %v", got)
	}
}

type nativeCleanupUncomparableError struct{ detail []byte }

func (nativeCleanupUncomparableError) Error() string { return "private cleanup failure" }

func TestIssuePreservesOperationalResolverCauseAndPureDenial(t *testing.T) {
	request := Request{AccountID: "usr_1", UserID: "usr_1", CLIClientSessionID: "cli_1", OperationID: "op_1", ResourceKind: "machine_service", ResourceID: "mch_1", RouteID: "tcp:22", Protocol: "tcp"}
	signer, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service := Service{Signer: signer, Issuer: "https://api.example.test"}
	secret := "private-database-detail"
	operational := errors.Join(ErrDenied, fmt.Errorf("query detail %s: %w", secret, syscall.ECONNREFUSED))
	service.Resolver = resolverFunc(func(context.Context, Request) (Target, error) { return Target{}, operational })
	if _, err = service.Issue(context.Background(), request); err != operational || !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("Issue discarded or replaced the resolver failure: %v", err)
	}
	if err = service.Revalidate(context.Background(), request, Target{}); err != operational || !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("Revalidate discarded or replaced the resolver failure: %v", err)
	}
	service.Resolver = resolverFunc(func(context.Context, Request) (Target, error) {
		return Target{}, fmt.Errorf("credential rejected: %w", ErrDenied)
	})
	if _, err = service.Issue(context.Background(), request); err != ErrDenied {
		t.Fatalf("pure expected denial = %v; want ErrDenied", err)
	}
}
func TestIssueAndRevalidateUseAuthoritativeExactTarget(t *testing.T) {
	now := time.Now().UTC()
	signer, _ := mint.NewEphemeral(time.Hour)
	req := Request{AccountID: "user_10000000-0000-4000-8000-000000000021", UserID: "user_10000000-0000-4000-8000-000000000021", CLIClientSessionID: "session_10000000-0000-4000-8000-000000000022", OperationID: "operation_10000000-0000-4000-8000-000000000023", ResourceKind: "machine_service", ResourceID: "machine_10000000-0000-4000-8000-000000000024", RouteID: "tcp:22", Protocol: "tcp"}
	target := Target{AccountID: req.AccountID, UserID: req.UserID, EnvironmentID: "machine_10000000-0000-4000-8000-000000000024", MachineID: "machine_10000000-0000-4000-8000-000000000024", AccessSessionID: "session_10000000-0000-4000-8000-000000000025", ResourceKind: req.ResourceKind, ResourceID: req.ResourceID, RouteID: req.RouteID, Protocol: req.Protocol, TargetScheme: "tcp", TargetAddress: "127.0.0.1:22", ResourceGeneration: 2, RouteGeneration: 3, TargetGeneration: 4}
	target.InstallationGeneration, target.PolicyGeneration, target.AnnouncementGeneration = 2, 3, 4
	target.BootID = "boot_10000000-0000-4000-8000-000000000026"
	target.CLIClientSessionID = req.CLIClientSessionID
	current := target
	service := Service{Resolver: resolverFunc(func(context.Context, Request) (Target, error) { return current, nil }), Signer: signer, Issuer: "https://api.example.test", Now: func() time.Time { return now }}
	grant, err := service.Issue(context.Background(), req)
	if err != nil || grant.Credential == "" || grant.Target != target {
		t.Fatalf("grant=%+v err=%v", grant, err)
	}
	if err = service.Revalidate(context.Background(), req, target); err != nil {
		t.Fatal(err)
	}
	current.TargetGeneration++
	if !errors.Is(service.Revalidate(context.Background(), req, target), ErrDenied) {
		t.Fatal("stale generation accepted")
	}
	current = target
	current.TargetAddress = "192.0.2.1:22"
	if _, err = service.Issue(context.Background(), req); !errors.Is(err, ErrDenied) {
		t.Fatal("non-loopback target issued")
	}
}
