package server

import (
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
)

func TestExpectedCredentialRejectionDoesNotHideMixedFailure(t *testing.T) {
	invalidCredential := &auth.Error{Code: auth.SignatureInvalid}
	if !expectedCredentialRejection(invalidCredential) {
		t.Fatal("invalid credential was not recognized as an expected rejection")
	}
	if !expectedCredentialRejection(errors.Join(invalidCredential, ErrCredentialPolicy)) {
		t.Fatal("joined expected credential rejections were not recognized")
	}
	if expectedCredentialRejection(errors.Join(invalidCredential, errors.New("authorization store unavailable"))) {
		t.Fatal("mixed credential rejection and operational failure were suppressed")
	}
	if expectedCredentialRejection(&auth.Error{Code: auth.KeyUnknown, Cause: errors.New("key lookup unavailable")}) {
		t.Fatal("key lookup failure was treated as an invalid credential")
	}
	if !expectedCredentialRejection(&auth.Error{Code: auth.Malformed, Cause: errors.New("malformed credential")}) {
		t.Fatal("malformed credential was not treated as an expected rejection")
	}
}
