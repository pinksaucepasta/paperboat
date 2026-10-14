package runtime

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/privateproxyconfig"
)

type failingPreviewPrivateAccess struct {
	starts int
	err    error
}

func (s *failingPreviewPrivateAccess) Start(context.Context) error {
	s.starts++
	return s.err
}

func TestStartPreviewPrivateAccessPreservesUserPACFailure(t *testing.T) {
	want := errors.New("interactive user registry unavailable")
	service := &failingPreviewPrivateAccess{err: want}
	if err := startPreviewPrivateAccess(context.Background(), service); !errors.Is(err, want) {
		t.Fatalf("isolated optional PAC start: %v", err)
	}
	if service.starts != 1 {
		t.Fatalf("starts=%d, want 1", service.starts)
	}
}

func TestStartPreviewPrivateAccessPreservesRequiredFailure(t *testing.T) {
	want := errors.New("PAC start failed")
	service := &failingPreviewPrivateAccess{err: want}
	if err := startPreviewPrivateAccess(context.Background(), service); !errors.Is(err, want) {
		t.Fatalf("start error=%v, want %v", err, want)
	}
}

func TestStartPreviewPrivateAccessAllowsHeadlessHost(t *testing.T) {
	service := &failingPreviewPrivateAccess{err: fmt.Errorf("desktop session unavailable: %w", privateproxyconfig.ErrUnsupported)}
	if err := startPreviewPrivateAccess(context.Background(), service); err != nil {
		t.Fatalf("headless public preview start: %v", err)
	}
	if service.starts != 1 {
		t.Fatalf("starts=%d, want 1", service.starts)
	}
}

func TestStartPreviewPrivateAccessDoesNotHideMixedUnsupported(t *testing.T) {
	service := &failingPreviewPrivateAccess{err: errors.Join(privateproxyconfig.ErrUnsupported, syscall.EIO)}
	if err := startPreviewPrivateAccess(t.Context(), service); !errors.Is(err, syscall.EIO) {
		t.Fatal("independent proxy failure suppressed")
	}
}
