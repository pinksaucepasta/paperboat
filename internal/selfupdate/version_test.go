package selfupdate

import "testing"

func TestCompareVersions(t *testing.T) {
	if comparison, err := CompareVersions("2026.08.18.1", "2026.08.18.0"); err != nil || comparison != 1 {
		t.Fatalf("comparison=%d error=%v", comparison, err)
	}
	if _, err := CompareVersions("latest", "2026.08.18.0"); err == nil {
		t.Fatal("malformed version was accepted")
	}
}
