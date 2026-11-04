package reporting

import (
	"context"
	"errors"
	"fmt"
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

type statusErr int

func (s statusErr) Error() string         { panic("must not be read") }
func (s statusErr) DiagnosticStatus() int { return int(s) }

func TestProjectRanksCausesAndRunsExtensions(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		cause string
		rej   bool
	}{
		{"rejection", statusErr(409), "conflict", true},
		{"refused beats cancel", errors.Join(context.Canceled, syscall.ECONNREFUSED), "connection_refused", false},
		{"deadline beats errno", errors.Join(syscall.ECONNREFUSED, context.DeadlineExceeded), "deadline_exceeded", false},
		{"unknown joined with cancel", errors.Join(context.Canceled, errors.New("x")), "internal", false},
		{"orderly cancel", fmt.Errorf("w: %w", context.Canceled), "context_canceled", false},
	}
	for _, tc := range cases {
		p := Project(tc.err, Options{})
		if p.Cause != tc.cause || p.Rejected != tc.rej {
			t.Errorf("%s: %+v", tc.name, p)
		}
	}
	visited := 0
	p := Project(errors.New("db"), Options{
		Classify: []NodeClassifier{func(error) (string, int, int) { return "postgres_deadlock", 0, 60 }},
		Visit:    func(error) { visited++ },
	})
	if p.Cause != "postgres_deadlock" || visited != 1 {
		t.Fatalf("extension: %+v visited=%d", p, visited)
	}
}
