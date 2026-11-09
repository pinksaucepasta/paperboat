package reportcause

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"github.com/pinksaucepasta/paperboat/shared/reporting"
)

func TestFaultCarriesSharedFields(t *testing.T) {
	if missing := reporting.MissingFaultFields(reporting.Fault{}); len(missing) != 0 {
		t.Fatalf("relay Fault lacks shared fields %v", missing)
	}
}

// A relay refusing an account over its bandwidth allowance reports that cause, whether the error is
// the relay's own value or a peer's typed error carrying its code.
func TestQuotaRefusalIsClassified(t *testing.T) {
	for _, err := range []error{derpquic.ErrQuota, &derpquic.Error{Code: derpquic.ErrQuota.Code, Message: "remote"}} {
		if cause, _ := classify(err); cause != "bandwidth_quota_exhausted" {
			t.Fatalf("%v classified as %q", err, cause)
		}
	}
}
