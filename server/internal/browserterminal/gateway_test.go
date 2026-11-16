package browserterminal

import (
	"fmt"
	"testing"
)

func TestGatewayAttachmentBoundAndRelease(t *testing.T) {
	gateway := Gateway{MaxPerSession: 8}
	for i := 0; i < 8; i++ {
		if !gateway.acquire("browser_session_1") {
			t.Fatalf("attachment %d rejected before limit", i)
		}
	}
	if gateway.acquire("browser_session_1") {
		t.Fatal("ninth attachment exceeded per-session limit")
	}
	if !gateway.acquire("browser_session_2") {
		t.Fatal("another browser session was blocked by per-session limit")
	}
	gateway.release("browser_session_1")
	if !gateway.acquire("browser_session_1") {
		t.Fatal("detached attachment did not release capacity")
	}
}

// F21-164: a Pro account holds 100 browser terminals through one console login, and logins of other accounts do not
// share a server-wide ceiling (it was 8 per login and 256 in all, far under the plan limits).
func TestGatewayAdmitsAPlanFullOfAttachmentsAcrossManyLogins(t *testing.T) {
	gateway := Gateway{MaxPerSession: 1000}
	for login := 0; login < 5; login++ {
		for i := 0; i < 100; i++ {
			if !gateway.acquire(fmt.Sprintf("browser_session_%d", login)) {
				t.Fatalf("login %d attachment %d rejected under a limit of 1000 per login", login, i)
			}
		}
	}
	for i := 100; i < 1000; i++ {
		gateway.acquire("browser_session_0")
	}
	if gateway.acquire("browser_session_0") {
		t.Fatal("the 1001st attachment of one login exceeded MaxPerSession")
	}
}
