package localdaemon

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

func TestNativeProbeRejectsOnlyProvenAuthorityFailures(t *testing.T) {
	for _, tc := range []struct {
		err    error
		denied bool
	}{
		{tailnet.ErrAdmission, true},
		{fmt.Errorf("admission rejected: %w", tailnet.ErrAuthority), true},
		{errors.Join(tailnet.ErrAdmission, tailnet.ErrAuthority, context.Canceled), true},
		{errors.Join(tailnet.ErrAdmission, syscall.EIO), false},
		{errors.Join(tailnet.ErrAuthority, context.DeadlineExceeded), false},
		{context.Canceled, false},
		{syscall.EIO, false},
	} {
		result := peerProbeFailure(context.Background(), tc.err)
		if localapi.IsPermissionFailure(result) != tc.denied || !errors.Is(result, tc.err) {
			t.Fatalf("probe classification or cause lost for %T", tc.err)
		}
		if !tc.denied && result != tc.err {
			t.Fatal("operational failure was converted to permission denial")
		}
	}
}
