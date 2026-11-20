package selfhost

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

// A claim carries the full JWKS and revocations, so it can exceed 1 MiB.
func TestDecodeAcceptsClaimLargerThanOneMiB(t *testing.T) {
	const size = 2 << 20
	body, err := json.Marshal(ClaimRequest{ClaimID: strings.Repeat("a", size)})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/selfhost/claim", bytes.NewReader(body))
	var got ClaimRequest
	if err := decode(w, r, &got, connectorprotocol.MaximumSnapshotBytes); err != nil {
		t.Fatalf("decode of a %d-byte claim failed: %v", len(body), err)
	}
	if len(got.ClaimID) != size {
		t.Fatalf("decoded ClaimID length %d, want %d", len(got.ClaimID), size)
	}
}
