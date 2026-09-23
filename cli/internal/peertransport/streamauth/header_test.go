package streamauth

import (
	"bytes"
	"testing"
	"time"
)

func TestHeaderCanonicalRoundTripAndGrant(t *testing.T) {
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
	grant := parsed.Grant()
	if grant.OperationID != header.OperationID || grant.Consumer != header.Consumer || grant.StreamID != header.StreamID || !bytes.Equal(grant.Credential, []byte(header.Credential)) || grant.MaximumBytes != header.MaximumBytes || !grant.Deadline.Equal(now.Add(time.Minute)) {
		t.Fatalf("grant=%+v", grant)
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
