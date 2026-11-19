package peernetwork

import (
	"reflect"
	"strings"
)

// A client strictly decodes a signed configuration (an unknown field fails the whole document, so a signed
// restriction is never silently dropped), so a field its build predates must not be in the document it receives.
// The server therefore omits, before signing, every field whose minimum capability version (shared/capver) is above
// the one the requesting client sent. Prior art: Tailscale tailcfg.CapabilityVersion gates MapResponse fields the
// same way (~/ref/tailscale/tailcfg/tailcfg.go). Incident 2026-11-19: home_relay was added with no gate and clients
// without it rejected every document.
//
// minCapVersion lists every json field of the signed types, "Type.json_name". Baseline fields are 1: a sender with no
// capability version is treated as 1. A field added later gets the capver.Version that first understands it, must be
// omitempty, and a test fails until the new field has an entry here.
var minCapVersion = map[string]int{
	"Configuration.version": 1, "Configuration.iss": 1, "Configuration.aud": 1, "Configuration.iat": 1,
	"Configuration.exp": 1, "Configuration.generation": 1, "Configuration.self": 1, "Configuration.peers": 1,
	"Binding.account_id": 1, "Binding.endpoint_id": 1, "Binding.role": 1, "Binding.machine_id": 1,
	"Binding.endpoint_generation": 1, "Binding.machine_generation": 1, "Binding.key_generation": 1,
	"Binding.wireguard_public_key": 1, "Binding.disco_public_key": 1, "Binding.quic_certificate_fingerprint": 1,
	"Binding.quic_public_key": 1, "Binding.virtual_address": 1,
	"Peer.identity": 1, "Peer.scopes": 1, "Peer.endpoints": 1,
	"Peer.home_relay":     2,
	"Scope.resource_kind": 1, "Scope.resource_id": 1, "Scope.resource_generation": 1, "Scope.capability": 1,
	"Scope.direction": 1, "Scope.port": 1, "Scope.expires_at": 1,
}

// ForCapVersion is c without the fields a client of capability version capVersion does not understand. c and its
// slices are not modified.
func (c Configuration) ForCapVersion(capVersion int) Configuration {
	omitNewer(reflect.ValueOf(&c).Elem(), max(capVersion, 1))
	return c
}

func omitNewer(v reflect.Value, capVersion int) {
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			if minCapVersion[v.Type().Name()+"."+jsonName(v.Type().Field(i))] > capVersion {
				v.Field(i).SetZero()
			} else {
				omitNewer(v.Field(i), capVersion)
			}
		}
	case reflect.Slice:
		if v.IsNil() {
			return
		}
		copied := reflect.MakeSlice(v.Type(), v.Len(), v.Len()) // the caller's backing array stays as it was
		reflect.Copy(copied, v)
		v.Set(copied)
		for i := range copied.Len() {
			omitNewer(copied.Index(i), capVersion)
		}
	}
}

// jsonName is the field's json key ("" for a field json skips).
func jsonName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	return name
}
