package peersessions

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// peerCarrierOffers is the carrier offer the last signed configuration gives each peer.
func (f endpointsFixture) peerCarrierOffers(t *testing.T) map[string][]byte {
	t.Helper()
	raw, err := json.Marshal(*f.signed)
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Self  map[string]any `json:"self"`
		Peers []struct {
			Identity     map[string]any `json:"identity"`
			CarrierOffer []byte         `json:"carrier_offer"`
		} `json:"peers"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	if _, ok := claims.Self["carrier_offer"]; ok {
		t.Fatal("the signed self identity carries a carrier offer")
	}
	out := map[string][]byte{}
	for _, p := range claims.Peers {
		if _, ok := p.Identity["carrier_offer"]; ok {
			t.Fatal("a signed peer identity carries a carrier offer")
		}
		out[p.Identity["endpoint_id"].(string)] = p.CarrierOffer
	}
	return out
}

// A machine that moved to a new carrier generation reports its signed offer; every peer's next configuration
// carries it unchanged on the machine's entry, with one peer_network move at once (no once-a-minute hold), until
// it expires or is withdrawn. An offer bound to another machine, installation generation or QUIC key is refused.
func TestCarrierOfferReachesPeersUnchangedOnPostgres(t *testing.T) {
	f := newEndpointsFixture(t)
	machine := f.account + "_m_1"
	start := time.Now().UTC()
	clock := start
	f.service.now = func() time.Time { return clock }
	quicKey := sha256.Sum256([]byte("key" + machine))
	offer := func(machineID string, generation int64, key []byte, expires time.Time) []byte {
		raw, err := json.Marshal(map[string]any{"Claims": map[string]any{"Kind": "paperboat.terminal-carrier/v1", "AccountID": f.account, "MachineID": machineID, "MachineGeneration": generation,
			"ParentPublicKey": base64.RawURLEncoding.EncodeToString(key), "ExpiresUnix": expires.Unix(), "Certificate": []byte("certificate"), "AllApplications": true}, "Signature": bytes.Repeat([]byte{7}, 64)})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	report := func(raw []byte) error {
		return f.service.ReportCarrierOffer(f.ctx, CarrierOfferReport{UserID: f.account, MachineID: machine, MachineGeneration: 1, Offer: raw})
	}

	f.configure(t, "before", 0, nil)
	if got := f.peerCarrierOffers(t)[machine]; got != nil {
		t.Fatalf("an offer before any report: %q", got)
	}
	current := offer(machine, 1, quicKey[:], start.Add(4*time.Minute))
	before := f.topic(t)
	if err := report(current); err != nil {
		t.Fatal(err)
	}
	if after := f.topic(t); after != before+1 {
		t.Fatalf("the report moved the peer_network topic %d times, want 1 at once", after-before)
	}
	f.configure(t, "moved", 0, nil)
	if got := f.peerCarrierOffers(t)[machine]; !bytes.Equal(got, current) {
		t.Fatalf("peer entry carries %q, want the reported offer unchanged", got)
	}
	if err := report(current); err != nil || f.topic(t) != before+1 {
		t.Fatalf("reporting the same offer again: err=%v topic moves %d", err, f.topic(t)-before)
	}

	other := sha256.Sum256([]byte("another key"))
	for name, raw := range map[string][]byte{
		"another machine":    offer(f.account+"_m_0", 1, quicKey[:], start.Add(4*time.Minute)),
		"another generation": offer(machine, 2, quicKey[:], start.Add(4*time.Minute)),
		"another QUIC key":   offer(machine, 1, other[:], start.Add(4*time.Minute)),
	} {
		if err := report(raw); !errors.Is(err, ErrDenied) {
			t.Fatalf("%s: err=%v, want ErrDenied", name, err)
		}
	}
	for name, raw := range map[string][]byte{
		"expired":      offer(machine, 1, quicKey[:], start.Add(-time.Second)),
		"too long":     offer(machine, 1, quicKey[:], start.Add(time.Hour)),
		"not an offer": []byte("not json"),
	} {
		if err := report(raw); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: err=%v, want ErrInvalid", name, err)
		}
	}
	f.configure(t, "kept", 0, nil)
	if got := f.peerCarrierOffers(t)[machine]; !bytes.Equal(got, current) {
		t.Fatal("a refused report replaced the stored offer")
	}

	clock = start.Add(4*time.Minute + time.Second)
	f.configure(t, "expired", 0, nil)
	if got := f.peerCarrierOffers(t)[machine]; got != nil {
		t.Fatal("an expired offer was delivered")
	}
	clock = start
	moved := f.topic(t)
	if err := report(nil); err != nil || f.topic(t) != moved+1 {
		t.Fatalf("withdrawal: err=%v topic moves %d", err, f.topic(t)-moved)
	}
	f.configure(t, "withdrawn", 0, nil)
	if got := f.peerCarrierOffers(t)[machine]; got != nil {
		t.Fatal("a withdrawn offer was delivered")
	}
}
