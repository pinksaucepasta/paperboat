package canonicaljson

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestRejectDuplicateFieldsKeepsMalformedJSONCausePrivate(t *testing.T) {
	for _, input := range []string{`{"token":"PRIVATE_VALUE", "field": @}`, `{"token":"PRIVATE_VALUE"} @`} {
		err := RejectDuplicateFields([]byte(input))
		if err == nil {
			t.Fatal("RejectDuplicateFields() error = nil, want malformed JSON error")
		}
		if got := err.Error(); got != "invalid JSON" {
			t.Fatalf("error text = %q, want a static safe message", got)
		}
		var syntaxErr *json.SyntaxError
		if !errors.As(err, &syntaxErr) {
			t.Fatalf("error %T does not preserve the JSON syntax cause", err)
		}
	}
}

func TestRejectDuplicateFieldsStillIdentifiesDuplicateKeys(t *testing.T) {
	err := RejectDuplicateFields([]byte(`{"field": 1, "field": 2}`))
	if !errors.Is(err, ErrDuplicateField) {
		t.Fatalf("error = %v, want ErrDuplicateField", err)
	}
}
