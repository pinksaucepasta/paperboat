package controlplane

import (
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
)

func TestPersistedEnrollmentReplayFailuresRetainCause(t *testing.T) {
	const key = "replay-fault-test-key"
	service := &EnrollmentService{encryptionKey: key}
	requestHash := sha256.Sum256([]byte("request"))

	for _, tc := range []struct {
		name       string
		grant      bool
		ciphertext func() []byte
		check      func(error) bool
	}{
		{
			name:       "grant decryption",
			grant:      true,
			ciphertext: func() []byte { return []byte("invalid-ciphertext") },
			check:      func(err error) bool { return errors.Is(err, secrets.ErrDecrypt) },
		},
		{
			name:  "grant decoding",
			grant: true,
			ciphertext: func() []byte {
				ciphertext, err := secrets.Encrypt(key, "{")
				if err != nil {
					t.Fatal(err)
				}
				return ciphertext
			},
			check: func(err error) bool { return err != nil && !errors.Is(err, ErrEnrollmentInvalid) },
		},
		{
			name:  "grant required fields",
			grant: true,
			ciphertext: func() []byte {
				ciphertext, err := secrets.Encrypt(key, `{}`)
				if err != nil {
					t.Fatal(err)
				}
				return ciphertext
			},
			check: func(err error) bool { return err != nil && !errors.Is(err, ErrEnrollmentInvalid) },
		},
		{
			name:       "identity decryption",
			ciphertext: func() []byte { return []byte("invalid-ciphertext") },
			check:      func(err error) bool { return errors.Is(err, secrets.ErrDecrypt) },
		},
		{
			name: "identity decoding",
			ciphertext: func() []byte {
				ciphertext, err := secrets.Encrypt(key, "{")
				if err != nil {
					t.Fatal(err)
				}
				return ciphertext
			},
			check: func(err error) bool { return err != nil && !errors.Is(err, ErrEnrollmentInvalid) },
		},
		{
			name: "identity required fields",
			ciphertext: func() []byte {
				ciphertext, err := secrets.Encrypt(key, `{}`)
				if err != nil {
					t.Fatal(err)
				}
				return ciphertext
			},
			check: func(err error) bool { return err != nil && !errors.Is(err, ErrEnrollmentInvalid) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ciphertext := tc.ciphertext()
			var err error
			if tc.grant {
				_, err = service.replayGrant(dbsqlc.ControlHelperEnrollment{RequestHash: requestHash[:], GrantCiphertext: ciphertext}, requestHash)
			} else {
				_, err = service.replayIdentityRenewal(requestHash[:], ciphertext, requestHash)
			}
			if !tc.check(err) {
				t.Fatalf("replay error=%v, did not preserve corruption cause", err)
			}
		})
	}
}
