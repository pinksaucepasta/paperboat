package configsync

import (
	"errors"
	"strings"
	"testing"
)

// A server that adds a response field must not stop older config workers.
func TestControlResponseIgnoresUnknownFieldsButNotTrailingData(t *testing.T) {
	var envelope struct {
		Data RuntimeDescriptor `json:"data"`
	}
	body := `{"data":{"machine_id":"m1","machine_alias":"pbvm1","added_later":{"x":1}}}`
	if err := decodeBoundedJSON(strings.NewReader(body), &envelope); err != nil {
		t.Fatalf("unknown field refused: %v", err)
	}
	if envelope.Data.MachineID != "m1" || envelope.Data.MachineAlias != "pbvm1" {
		t.Fatalf("decoded %+v", envelope.Data)
	}
	if err := decodeBoundedJSON(strings.NewReader(body+`{}`), &envelope); !errors.Is(err, ErrControlClientInvalid) {
		t.Fatalf("trailing document accepted: %v", err)
	}
	if err := decodeBoundedJSON(strings.NewReader(`{"data":{"machine_id":7}}`), &envelope); err == nil {
		t.Fatal("wrong field type accepted")
	}
}
