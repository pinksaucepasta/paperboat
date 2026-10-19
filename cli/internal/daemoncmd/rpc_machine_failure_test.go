package daemoncmd

import (
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// F9 / pb resolve: an unknown machine is the shared "no machine has that name" error, not the
// opaque operation_failed every daemon failure used to become.
func TestRPCMachineFailurePresentsUnknownMachineAndHidesOtherCauses(t *testing.T) {
	notFound := rpcMachineFailure("generic", "dadape", status.Error(codes.NotFound, "machine not found (support reference: x)"))
	if !errors.Is(notFound, resolver.ErrMachineNotFound) {
		t.Fatalf("NotFound became %v, want the shared unknown-machine error", notFound)
	}
	other := rpcMachineFailure("generic", "dadape", status.Error(codes.Unavailable, "daemon down"))
	if errors.Is(other, resolver.ErrMachineNotFound) || other.Error() != "generic" {
		t.Fatalf("an unrelated failure became %v, want the generic coordinator message", other)
	}
	if rpcMachineFailure("generic", "dadape", nil) != nil {
		t.Fatal("no error became an error")
	}
}
