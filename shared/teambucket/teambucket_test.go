package teambucket

import (
	"errors"
	"strings"
	"testing"
)

func valid() Input {
	return Input{Endpoint: "https://s3.example.test", Region: "us-east-1", Bucket: "team-bucket", Prefix: "paperboat", AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "s"}
}

func TestInputValidate(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Input){
		"http endpoint":      func(in *Input) { in.Endpoint = "http://s3.example.test" },
		"endpoint with path": func(in *Input) { in.Endpoint = "https://s3.example.test/bucket" },
		"endpoint with key":  func(in *Input) { in.Endpoint = "https://user:pw@s3.example.test" },
		"bad bucket":         func(in *Input) { in.Bucket = "UPPER" },
		"prefix escapes":     func(in *Input) { in.Prefix = "../x" },
		"prefix empty part":  func(in *Input) { in.Prefix = "a//b" },
		"bad access key":     func(in *Input) { in.AccessKeyID = "has space" },
		"localhost":          func(in *Input) { in.Endpoint = "https://localhost" },
		"private address":    func(in *Input) { in.Endpoint = "https://10.0.0.5" },
		"metadata address":   func(in *Input) { in.Endpoint = "https://169.254.169.254" },
		"one-label host":     func(in *Input) { in.Endpoint = "https://minio" },
	} {
		in := valid()
		change(&in)
		if err := in.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	empty := valid()
	empty.Prefix = ""
	if empty.Validate() != nil {
		t.Error("an empty prefix is the bucket root")
	}
}

func TestObjectKeysStayUnderThePrefix(t *testing.T) {
	const id = "rec_00000000-0000-4000-8000-000000000001"
	key, err := RecordingSegmentKey("paperboat/", id, 7)
	if err != nil || key != "paperboat/v1/recordings/"+id+"/000007.seg" {
		t.Fatal(key, err)
	}
	if k, _ := RecordingSegmentKey("", id, 0); k != "v1/recordings/"+id+"/000000.seg" {
		t.Fatal(k)
	}
	for _, bad := range []string{"rec_x", id + "/../x", "../" + id} {
		if _, err := RecordingSegmentKey("p", bad, 0); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := RecordingSegmentKey("p", id, 1000000); err == nil {
		t.Error("segment index bound")
	}
}

func TestSegmentValidate(t *testing.T) {
	ok := Segment{Index: 0, Bytes: 10, SHA256: strings.Repeat("a", 64)}
	if ok.Validate() != nil {
		t.Fatal("valid segment refused")
	}
	for _, bad := range []Segment{{Index: -1, Bytes: 1, SHA256: ok.SHA256}, {Bytes: 0, SHA256: ok.SHA256}, {Bytes: MaxObjectBytes + 1, SHA256: ok.SHA256}, {Bytes: 1, SHA256: "AB"}} {
		if bad.Validate() == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}
