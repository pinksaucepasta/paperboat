//go:build windows

package windowsopenssh

import (
	"context"
	"flag"
	"testing"
	"time"
)

var nativeSSHGeneration = flag.String("task23-ssh-generation", "", "explicit task-owned prepared SSH generation")

// This is read-only against an explicitly prepared native task installation.
// The owning update qualification separately runs an authenticated SSH command
// across source activation and verifies the carrier's old bridge counts.
func TestNativeSSHGenerationCustody(t *testing.T) {
	if *nativeSSHGeneration == "" || *nativeQueryOwner == "" {
		t.Skip("explicit task-owned SSH generation and owner SID required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	descriptor, err := LoadGeneration(ctx, *nativeQueryOwner, *nativeSSHGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if err := InspectGeneration(ctx, descriptor); err != nil {
		t.Fatal(err)
	}
	altered := descriptor
	altered.Port++
	if err := InspectGeneration(ctx, altered); err == nil {
		t.Fatal("accepted a caller-modified target instead of the protected native listener")
	}
}
