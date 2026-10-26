package streamauth

import (
	"bytes"
	"testing"
	"time"
)

func TestHeaderCanonicalRoundTrip(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	header, err := New("operation_1", "exec", "native-control", "signed.credential.value", now.Add(time.Minute), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := header.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(encoded, now)
	if err != nil || parsed != header {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}
}

func TestHeaderRejectsExpiredNoncanonicalUnknownAndOversizedInput(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	header, _ := New("operation_1", "exec", "native-control", "credential", now.Add(time.Minute), 1)
	encoded, _ := header.MarshalBinary()
	for _, invalid := range [][]byte{
		append([]byte(" "), encoded...),
		bytes.Replace(encoded, []byte(`"version":1`), []byte(`"version":1,"unknown":1`), 1),
		bytes.Repeat([]byte{'x'}, MaximumHeaderSize+1),
	} {
		if _, err := Parse(invalid, now); err == nil {
			t.Fatalf("accepted %q", invalid[:min(len(invalid), 64)])
		}
	}
	header.DeadlineUnix = now.Unix()
	expired, _ := header.MarshalBinary()
	if _, err := Parse(expired, now); err == nil {
		t.Fatal("accepted expired header")
	}
}

func TestNativePrivateHeaderRequiresBoundedCanonicalTarget(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	target := []byte(`{"schema":"paperboat.native-private-target/v1"}`)
	header, err := NewNativePrivate("operation_private", "private_tcp", "stream_private", "credential", now.Add(time.Minute), 1024, target)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := header.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(encoded, now)
	if err != nil || parsed.Target != string(target) {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}
	header.Target = ""
	if header.Validate(now) == nil {
		t.Fatal("private stream without target accepted")
	}
	normal, _ := New("operation_terminal", "terminal", "stream_terminal", "credential", now.Add(time.Minute), 1024)
	normal.Target = string(target)
	if normal.Validate(now) == nil {
		t.Fatal("non-private stream with target accepted")
	}
}

// An offline header carries no credential and names only a session the host may start without
// Paperboat; an online header must carry its credential.
func TestOfflineHeaderCarriesNoCredentialAndOnlySessionConsumers(t *testing.T) {
	deadline := time.Now().Add(time.Minute)
	for _, consumer := range []string{"terminal", "exec", "ssh"} {
		header, err := NewOffline("op_1", consumer, "stream_1", deadline, 1<<20)
		if err != nil {
			t.Fatalf("%s: %v", consumer, err)
		}
		encoded, err := header.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := Parse(encoded, time.Now())
		if err != nil || !parsed.Offline || parsed.Credential != "" {
			t.Fatalf("%s: parsed %+v, %v", consumer, parsed, err)
		}
	}
	for _, consumer := range []string{"file_transfer", "private_tcp", "config_compare", "inspector"} {
		if _, err := NewOffline("op_1", consumer, "stream_1", deadline, 1<<20); err == nil {
			t.Fatalf("an offline %s header was accepted", consumer)
		}
	}
	withCredential := Header{Version: Version, OperationID: "op", Consumer: "exec", StreamID: "s", Credential: "token", DeadlineUnix: deadline.Unix(), MaximumBytes: 1, Offline: true}
	if withCredential.Validate(time.Time{}) == nil {
		t.Fatal("an offline header with a credential was accepted")
	}
	if _, err := New("op", "exec", "s", "", deadline, 1); err == nil {
		t.Fatal("an online header without a credential was accepted")
	}
}
