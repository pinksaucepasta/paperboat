package reportcause

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/reporting"
)

func TestFaultCarriesSharedFields(t *testing.T) {
	if missing := reporting.MissingFaultFields(reporting.Fault{}); len(missing) != 0 {
		t.Fatalf("tunnel Fault lacks shared fields %v", missing)
	}
}
