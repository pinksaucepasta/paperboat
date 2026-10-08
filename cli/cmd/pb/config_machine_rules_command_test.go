package main

import (
	"github.com/pinksaucepasta/paperboat/internal/api"
	"strings"
	"testing"
)

func TestDecodeConfigInputRejectsTrailingAndOversize(t *testing.T) {
	for _, raw := range []string{`[] []`, strings.Repeat(" ", configScopeLimit+1)} {
		var value []api.ConfigPathRule
		if decodeConfigInput(strings.NewReader(raw), &value) == nil {
			t.Fatal("accepted invalid bounded input")
		}
	}
}
