package controlplane

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestVerifyHelperProofBindsRequestAndIdentity(t *testing.T) {
	privateKey := ed25519.NewKeyFromSeed([]byte(strings.Repeat("p", ed25519.SeedSize)))
	publicKey := privateKey.Public().(ed25519.PublicKey)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"edge_pool":"default"}`)
	bodyHash := sha256.Sum256(body)
	claims := HelperProofClaims{HelperID: "hlp_1", EnvironmentID: "env_1", OperationID: "operation_1", Method: "POST", Path: "/v1/connectors/admission", BodySHA256: base64.RawURLEncoding.EncodeToString(bodyHash[:]), IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(helperProofEnvelope{Algorithm: "EdDSA", Payload: base64.RawURLEncoding.EncodeToString(payload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload))})
	if err != nil {
		t.Fatal(err)
	}
	verified, err := verifyHelperProof(publicKey, envelope, "hlp_1", "env_1", "POST", "/v1/connectors/admission", body, now)
	if err != nil || verified.OperationID != "operation_1" {
		t.Fatalf("verified = %#v, %v", verified, err)
	}
	if _, err := verifyHelperProof(publicKey, envelope, "hlp_1", "env_1", "POST", "/v1/connectors/admission", []byte(`{}`), now); !errors.Is(err, ErrHelperProof) {
		t.Fatalf("relabeled body error = %v", err)
	}
	if _, err := verifyHelperProof(publicKey, envelope, "hlp_1", "env_1", "POST", "/v1/connectors/admission", body, now.Add(2*time.Minute)); !errors.Is(err, ErrHelperProof) {
		t.Fatalf("expired proof error = %v", err)
	}
}

func TestStrictHelperProofJSONRejectsDuplicateKeys(t *testing.T) {
	var claims HelperProofClaims
	if err := strictProofJSON([]byte(`{"helper_id":"hlp_1","helper_id":"hlp_2"}`), &claims); !errors.Is(err, ErrHelperProof) {
		t.Fatalf("duplicate proof error = %v", err)
	}
}

// A correctly signed proof from a machine whose clock is off is clock skew, not a plain denial, so the
// client is told to correct its clock instead of being treated as signed out. A malformed window stays
// a plain denial.
func TestProofWindowSeparatesClockSkewFromMalformedProofs(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	for name, test := range map[string]struct {
		issued, expires time.Time
		want            error
	}{
		"current":                {now, now.Add(time.Minute), nil},
		"clock 90 s ahead":       {now.Add(90 * time.Second), now.Add(150 * time.Second), ErrClockSkew},
		"clock 2 min behind":     {now.Add(-2 * time.Minute), now.Add(-time.Minute), ErrClockSkew},
		"lifetime over a minute": {now, now.Add(2 * time.Minute), ErrHelperProof},
		"expires before issue":   {now, now.Add(-time.Second), ErrHelperProof},
		"no issue time":          {time.Time{}, now.Add(time.Minute), ErrHelperProof},
	} {
		err := proofWindow(test.issued, test.expires, now)
		if test.want == nil && err != nil || test.want != nil && !errors.Is(err, test.want) {
			t.Errorf("%s: %v, want %v", name, err, test.want)
		}
		if errors.Is(test.want, ErrHelperProof) && test.want != ErrClockSkew && errors.Is(err, ErrClockSkew) {
			t.Errorf("%s: malformed window reported as clock skew", name)
		}
		if err != nil && !errors.Is(err, ErrHelperProof) {
			t.Errorf("%s: %v is not a proof denial", name, err)
		}
	}
}
