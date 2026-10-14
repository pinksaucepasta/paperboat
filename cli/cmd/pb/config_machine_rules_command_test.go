package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

type configInputFailureReader struct {
	data []byte
	err  error
}

func (r configInputFailureReader) Read(p []byte) (int, error) {
	return copy(p, r.data), r.err
}

func TestDecodeConfigInputRejectsTrailingAndOversize(t *testing.T) {
	for _, raw := range []string{`[] []`, strings.Repeat(" ", configScopeLimit+1)} {
		var value []api.ConfigPathRule
		if decodeConfigInput(strings.NewReader(raw), &value) == nil {
			t.Fatal("accepted invalid bounded input")
		}
	}
}

func TestDecodeConfigInputKeepsSafeParserAndReadCauses(t *testing.T) {
	var value map[string]string
	parseErr := decodeConfigInput(strings.NewReader(`{"password":? "PRIVATE_VALUE"}`), &value)
	var syntaxErr *json.SyntaxError
	if !errors.As(parseErr, &syntaxErr) || !errors.Is(parseErr, errUsage) {
		t.Fatalf("JSON parse cause/usage classification lost: %T", parseErr)
	}
	if strings.Contains(parseErr.Error(), "PRIVATE_VALUE") {
		t.Fatalf("parser details escaped: %q", parseErr.Error())
	}

	readFailure := errors.New("PRIVATE_READ_FAILURE")
	readErr := decodeConfigInput(configInputFailureReader{data: []byte("PRIVATE_DATA"), err: readFailure}, &value)
	if !errors.Is(readErr, readFailure) || errors.Is(readErr, errUsage) {
		t.Fatalf("operational read cause/classification lost: %T", readErr)
	}
	if strings.Contains(readErr.Error(), "PRIVATE") {
		t.Fatalf("read details escaped: %q", readErr.Error())
	}

	var trailing map[string]string
	trailingErr := decodeConfigInput(strings.NewReader(`{} {}`), &trailing)
	if trailingErr == nil || !errors.Is(trailingErr, errUsage) {
		t.Fatalf("trailing JSON classification lost: %T", trailingErr)
	}
}
