//go:build linux || darwin

package sessionowner

import (
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/service"
)

type markerController struct{ service.Controller }

func TestCatalogProcessHookReplacesTheServiceManagerController(t *testing.T) {
	d := Descriptor{Generation: strings.Repeat("a", 32), Instance: "u1", UID: 1}
	want := markerController{}
	hooked := Catalog{Process: func(got Descriptor) (service.Controller, error) {
		if got.Generation != d.Generation {
			t.Fatalf("hook got %q", got.Generation)
		}
		return want, nil
	}}
	if got, err := hooked.controller(d); err != nil || got != service.Controller(want) {
		t.Fatalf("controller = %v, %v", got, err)
	}
	failing := Catalog{Process: func(Descriptor) (service.Controller, error) { return nil, ErrInvalid }}
	if _, err := failing.controller(d); !errors.Is(err, ErrInvalid) {
		t.Fatalf("hook error lost: %v", err)
	}
}

func TestOwnerCommandIsTheOneStartDeclaration(t *testing.T) {
	d := Descriptor{Generation: strings.Repeat("b", 32), RuntimeRoot: "/state", Workspace: "/ws", Shell: "/bin/sh"}
	args, environment := Catalog{Root: "/catalog"}.OwnerCommand(d)
	if strings.Join(args, " ") != "daemon __runtime-session-owner --generation "+d.Generation {
		t.Fatalf("args = %v", args)
	}
	want := map[string]string{"PAPERBOAT_SESSION_OWNER_CATALOG": "/catalog", "PAPERBOAT_RUNTIME_STATE_ROOT": "/state", "PAPERBOAT_WORKSPACE_ROOT": "/ws", "PAPERBOAT_SHELL": "/bin/sh"}
	for key, value := range want {
		if environment[key] != value {
			t.Fatalf("%s = %q, want %q", key, environment[key], value)
		}
	}
	if len(environment) != len(want) {
		t.Fatalf("unexpected environment %v", environment)
	}
}

func TestCatalogWithoutProcessHookUsesTheServiceManagerController(t *testing.T) {
	d := Descriptor{Source: installsource.Source{Platform: "linux"}, Generation: strings.Repeat("a", 32), Instance: "u1", UID: 1}
	if _, err := (Catalog{}).controller(d); !errors.Is(err, service.ErrInvalidDefinition) {
		t.Fatalf("a catalog with no runner got %v, want the native controller's refusal", err)
	}
	got, err := (Catalog{Runner: service.ExecRunner{}}).controller(d)
	systemd, ok := got.(service.SystemdController)
	if err != nil || !ok || systemd.Unit != "paperboat-session-owner-u1-"+d.Generation+".service" {
		t.Fatalf("controller = %#v, %v", got, err)
	}
}
