package runtime

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

type orderedComponent struct {
	name   string
	events *[]string
	mu     *sync.Mutex
	fail   bool
}

func (c orderedComponent) Start(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	*c.events = append(*c.events, "start:"+c.name)
	if c.fail {
		return errors.New("failed")
	}
	return nil
}
func (c orderedComponent) Shutdown(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	*c.events = append(*c.events, "stop:"+c.name)
	return nil
}

func TestDataPlaneOrdersStartupAndAccountingSafeShutdown(t *testing.T) {
	var mu sync.Mutex
	var events []string
	component := func(name string) Component { return orderedComponent{name: name, events: &events, mu: &mu} }
	dataPlane, err := NewDataPlane(DataPlaneSpec{Persistence: component("store"), Control: component("control"), Node: component("node"), Routes: component("routes"), Gateway: component("gateway"), HTTP3Ingress: component("http3"), PrivateIngress: component("private"), RedirectIngress: component("redirect"), Usage: component("usage")})
	if err != nil {
		t.Fatal(err)
	}
	if err := dataPlane.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := dataPlane.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"start:store", "start:gateway", "start:http3", "start:private", "start:redirect", "start:control", "start:node", "start:routes", "start:usage", "stop:usage", "stop:routes", "stop:node", "stop:control", "stop:redirect", "stop:private", "stop:http3", "stop:gateway", "stop:store"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v", events)
	}
}

func TestDataPlaneCleansPartialStartupInReverse(t *testing.T) {
	var mu sync.Mutex
	var events []string
	component := func(name string, fail bool) Component {
		return orderedComponent{name: name, events: &events, mu: &mu, fail: fail}
	}
	dataPlane, _ := NewDataPlane(DataPlaneSpec{Persistence: component("store", false), Control: component("control", false), Node: component("node", false), Routes: component("routes", false), Gateway: component("gateway", false), HTTP3Ingress: component("http3", true), PrivateIngress: component("private", false), RedirectIngress: component("redirect", false), Usage: component("usage", false)})
	if err := dataPlane.Start(context.Background()); err == nil {
		t.Fatal("partial startup succeeded")
	}
	want := []string{"start:store", "start:gateway", "start:http3", "stop:gateway", "stop:store"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v", events)
	}
}

func TestDataPlaneInstallsPreviewBeforeCarrierAndDetachesBeforeClose(t *testing.T) {
	var mu sync.Mutex
	var events []string
	component := func(name string) Component { return orderedComponent{name: name, events: &events, mu: &mu} }
	dataPlane, err := NewDataPlane(DataPlaneSpec{
		Persistence: component("store"), Control: component("control"), Carrier: component("carrier"), Preview: component("preview"),
		Node: component("node"), Routes: component("routes"), Gateway: component("gateway"),
		HTTP3Ingress: component("http3"), PrivateIngress: component("private"), RedirectIngress: component("redirect"), Usage: component("usage"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dataPlane.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := dataPlane.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	previewStart, carrierStart := indexOf(events, "start:preview"), indexOf(events, "start:carrier")
	previewStop, carrierStop := indexOf(events, "stop:preview"), indexOf(events, "stop:carrier")
	if previewStart < 0 || carrierStart < 0 || previewStart > carrierStart || previewStop < 0 || carrierStop < 0 || previewStop > carrierStop {
		t.Fatalf("preview/carrier lifecycle order = %v", events)
	}
}

func indexOf(values []string, want string) int {
	for index, value := range values {
		if value == want {
			return index
		}
	}
	return -1
}
