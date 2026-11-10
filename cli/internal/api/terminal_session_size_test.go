package api

import (
	"encoding/json"
	"testing"
)

func TestTerminalSessionDecodesTheLiveSizeAndToleratesItsAbsence(t *testing.T) {
	var sized, bare TerminalSession
	if err := json.Unmarshal([]byte(`{"id":"s1","columns":100,"rows":30,"unknown_later":true}`), &sized); err != nil {
		t.Fatal(err)
	}
	if sized.Columns == nil || sized.Rows == nil || *sized.Columns != 100 || *sized.Rows != 30 {
		t.Fatalf("size not decoded: %+v %+v", sized.Columns, sized.Rows)
	}
	if err := json.Unmarshal([]byte(`{"id":"s2"}`), &bare); err != nil || bare.Columns != nil || bare.Rows != nil {
		t.Fatalf("an absent size must stay unknown, not 0: %+v %+v %v", bare.Columns, bare.Rows, err)
	}
	out, _ := json.Marshal(bare)
	var back map[string]any
	_ = json.Unmarshal(out, &back)
	if _, present := back["columns"]; present {
		t.Fatalf("an unknown size is printed as %v", back["columns"])
	}
}
