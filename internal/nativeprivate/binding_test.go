package nativeprivate

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func validBinding(now time.Time) Binding {
	return Binding{Schema: SchemaV1, ResourceKind: "tunnel", ResourceID: "tun_1", ResourceGeneration: 2, RouteID: "route_1", RouteGeneration: 3, TargetGeneration: 4, OwnerEndpointID: "machine_1", Protocol: "tcp", TargetScheme: "tcp", TargetAddress: "127.0.0.1:22", ExpiresAt: now.Add(time.Minute)}
}

func TestBindingRequiresExactCurrentLoopbackTarget(t *testing.T) {
	now := time.Now().UTC()
	for name, mutate := range map[string]func(*Binding){
		"resource":              func(v *Binding) { v.ResourceID = "" },
		"resource generation":   func(v *Binding) { v.ResourceGeneration = 0 },
		"route generation":      func(v *Binding) { v.RouteGeneration = 0 },
		"target generation":     func(v *Binding) { v.TargetGeneration = 0 },
		"owner":                 func(v *Binding) { v.OwnerEndpointID = "" },
		"protocol substitution": func(v *Binding) { v.Protocol = "http" },
		"network target":        func(v *Binding) { v.TargetAddress = "192.0.2.1:22" },
		"hostname target":       func(v *Binding) { v.TargetAddress = "localhost:22" },
		"expired":               func(v *Binding) { v.ExpiresAt = now },
	} {
		t.Run(name, func(t *testing.T) {
			value := validBinding(now)
			mutate(&value)
			if !errors.Is(value.Validate(now), ErrInvalid) {
				t.Fatalf("invalid binding accepted: %#v", value)
			}
		})
	}
}

func TestDecodeIsStrictAndBounded(t *testing.T) {
	now := time.Now().UTC()
	raw, _ := json.Marshal(validBinding(now))
	if _, err := Decode(raw, now); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]byte{append(raw, []byte(" {}")...), []byte(`{"schema":"paperboat.native-private-target/v1","unknown":true}`), make([]byte, 4097)} {
		if !errors.Is(func() error { _, err := Decode(invalid, now); return err }(), ErrInvalid) {
			t.Fatal("invalid document accepted")
		}
	}
}
