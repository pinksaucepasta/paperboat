package browserterminal

import "testing"

func TestGatewayAttachmentBoundAndRelease(t *testing.T) {
	var gateway Gateway
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
