package hostinstall

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type suppliedSourcePrivateError struct{}

func (*suppliedSourcePrivateError) Error() string { panic("private source cause formatted") }

func TestSuppliedSourceConsumerPreservesVerificationCauseAndRecovery(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := installsource.Inspect(executable, "fixture", installsource.Custom)
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "private-missing-source")
	original := source.Verify(missing)
	if original == nil {
		t.Fatal("missing source accepted")
	}
	failure := suppliedRuntimeSourceFailure{cause: original}
	var pathCause *os.PathError
	if !errors.Is(failure, ErrInvalidRequest) || !errors.Is(failure, installsource.ErrInvalid) || !errors.Is(failure, os.ErrNotExist) || !errors.As(failure, &pathCause) {
		t.Fatal("consumer lost original verification cause")
	}
	if strings.Contains(failure.Error(), "private-missing-source") {
		t.Fatal("source path exposed")
	}
	spy := &suppliedSourcePrivateError{}
	wrapped := suppliedRuntimeSourceFailure{cause: errors.Join(installsource.ErrInvalid, spy)}
	if !errors.Is(wrapped, spy) {
		t.Fatal("independent cause lost")
	}
	_ = wrapped.Error()
	if err := source.Verify(executable); err != nil {
		t.Fatal("fresh original source did not recover")
	}
}
