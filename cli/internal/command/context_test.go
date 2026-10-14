package command

import (
	"flag"
	"testing"
)

func TestStringOptionalFlagPreservesRegisteredValue(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.String("server", "control.example", "")
	ctx := NewContext(flags)
	if ctx.String("workspace") != "" || ctx.String("server") != "control.example" {
		t.Fatal("optional string lookup changed configured value")
	}
	if NewContext(nil).String("workspace") != "" {
		t.Fatal("empty flag context has an optional value")
	}
}
