package preview

import (
	"testing"
	"time"
)

// F21-168: a background preview renews once a minute (the server forgives 5 minutes), a foreground one every 10 s
// (the server forgives 30 s).
func TestRenewIntervalFitsTheServersGraceForTheKindOfPreview(t *testing.T) {
	deadline := time.Now().Add(time.Hour)
	if got := renewIntervalFor(OwnerSessionLocalLease, &deadline); got != PreviewLeaseBackgroundRenewInterval {
		t.Fatalf("background preview renews every %s", got)
	}
	if got := renewIntervalFor(OwnerSessionLocalLease, nil); got != PreviewLeaseDefaultRenewInterval {
		t.Fatalf("local lease without a deadline renews every %s", got)
	}
	if got := renewIntervalFor("foreground", &deadline); got != PreviewLeaseDefaultRenewInterval {
		t.Fatalf("foreground preview renews every %s", got)
	}
	if PreviewLeaseBackgroundRenewInterval*4 > 5*time.Minute {
		t.Fatal("a background preview must get several renewals inside the server's 5 minute grace")
	}
}
