package reporting

import (
	"reflect"
	"syscall"
	"testing"
)

func TestProjectionPrimitives(t *testing.T) {
	if StatusCause(404) != "not_found" || StatusCause(418) != "invalid_request" || StatusCause(502) != "upstream_failure" || StatusCause(200) != "internal" {
		t.Fatal("status causes")
	}
	if ErrnoCause(syscall.ECONNREFUSED) != "connection_refused" || ErrnoCause(syscall.EEXIST) != "conflict" || ErrnoCause(syscall.Errno(9999)) != "system_error" {
		t.Fatal("errno causes")
	}
	if got := MissingFaultFields(Fault{}); len(got) != 0 {
		t.Fatalf("shared Fault lacks %v", got)
	}
	if got := MissingFaultFields(struct{ Component, HTTPStatus string }{}); len(got) != len(RequiredFaultFields)-2 {
		t.Fatalf("missing=%v", got)
	}
	if TypeName(reflect.TypeOf(&Fault{})) != "Fault" {
		t.Fatal("type name")
	}
}
