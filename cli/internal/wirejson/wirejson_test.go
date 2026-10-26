package wirejson

import (
	"errors"
	"testing"
)

type document struct {
	Name  string            `json:"name"`
	Inner struct{ A int }   `json:"inner"`
	List  []struct{ B int } `json:"list"`
}

func TestDecodeIgnoresUnknownFieldsAtEveryLevel(t *testing.T) {
	var out document
	raw := `{"name":"x","new_field":{"a":1},"inner":{"A":2,"extra":true},"list":[{"B":3,"extra":"y"}]}`
	if err := Decode([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	if out.Name != "x" || out.Inner.A != 2 || len(out.List) != 1 || out.List[0].B != 3 {
		t.Fatalf("decoded %+v", out)
	}
}

func TestDecodeStillRejectsWrongTypesAndTrailingData(t *testing.T) {
	var out document
	if err := Decode([]byte(`{"name":5}`), &out); err == nil {
		t.Fatal("wrong field type accepted")
	}
	if err := Decode([]byte(`{"name":"x"} {"name":"y"}`), &out); !errors.Is(err, ErrTrailingData) {
		t.Fatalf("trailing data: %v", err)
	}
}
