package command

import (
	"flag"
	"strings"
	"testing"
)

func TestStringOptionalFlagPreservesRegisteredValue(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.String("server", "control.example", "")
	ctx := NewContext(flags, strings.NewReader(""))
	if ctx.String("workspace") != "" || ctx.String("server") != "control.example" {
		t.Fatal("optional string lookup changed configured value")
	}
	if NewContext(nil, strings.NewReader("")).String("workspace") != "" {
		t.Fatal("empty flag context has an optional value")
	}
}
