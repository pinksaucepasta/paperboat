package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

func TestAServer426DropsTheCachedUpdatePolicy(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "policy.json")
	write := func() {
		if err := os.WriteFile(cache, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	forgetUpdatePolicyAfterRefusal(cache, errors.New("other"))
	forgetUpdatePolicyAfterRefusal(cache, &api.APIError{Status: http.StatusConflict, Code: "update_required"})
	forgetUpdatePolicyAfterRefusal(cache, &api.APIError{Status: http.StatusUpgradeRequired, Code: "other"})
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("the cache was dropped for an unrelated failure: %v", err)
	}
	forgetUpdatePolicyAfterRefusal(cache, errors.Join(errors.New("wrapped"), &api.APIError{Status: http.StatusUpgradeRequired, Code: "update_required"}))
	if _, err := os.Stat(cache); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a server update_required refusal kept the stale cache: %v", err)
	}
	forgetUpdatePolicyAfterRefusal("", &api.APIError{Status: http.StatusUpgradeRequired, Code: "update_required"}) // no path: nothing happens
}
