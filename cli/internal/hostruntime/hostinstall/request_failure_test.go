package hostinstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"syscall"
	"testing"
)

func TestDecodePreservesFailureAndEnforcesCompleteRequestBound(t *testing.T) {
	t.Run("read cause", func(t *testing.T) {
		formatted := 0
		_, err := Decode(io.MultiReader(strings.NewReader(`{"schema":`), failedInstallReader{formatted: &formatted}))
		if !errors.Is(err, syscall.EIO) || !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("read cause lost: %T", err)
		}
		if formatted != 0 || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal("reader content exposed")
		}
	})
	t.Run("typed parser", func(t *testing.T) {
		_, err := Decode(strings.NewReader(`{"schema":123}`))
		var syntax *json.UnmarshalTypeError
		if !errors.As(err, &syntax) || !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("parser cause lost: %T", err)
		}
	})
	t.Run("all bytes bounded", func(t *testing.T) {
		payload := append([]byte(`{}`), bytes.Repeat([]byte(" "), 128<<10)...)
		if _, err := Decode(bytes.NewReader(payload)); !errors.Is(err, ErrInvalidRequest) {
			t.Fatal("oversized request accepted")
		}
		r, err := Decode(strings.NewReader(`{"schema":"paperboat.host-install/v1"}`))
		if err != nil || r.Schema != SchemaV1 {
			t.Fatal("fresh valid request did not recover")
		}
	})
}

type failedInstallReader struct{ formatted *int }

func (r failedInstallReader) Read([]byte) (int, error) {
	return 0, privateInstallReadFailure{formatted: r.formatted}
}

type privateInstallReadFailure struct{ formatted *int }

func (e privateInstallReadFailure) Error() string { *e.formatted++; return "PRIVATE reader data" }
func (e privateInstallReadFailure) Unwrap() error { return syscall.EIO }
