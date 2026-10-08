package hostruntimecmd

import "testing"

func TestBootstrapSSHFields(t *testing.T) {
	if user, port := bootstrapSSHFields(" pujan ", 22); user != "pujan" || port != 22 {
		t.Fatalf("SSH fields = %q/%d", user, port)
	}
	if user, port := bootstrapSSHFields("", 0); user != "" || port != 0 {
		t.Fatalf("optional SSH fields = %q/%d", user, port)
	}
}
func TestUnixBootstrapSSHFields(t *testing.T) {
	if user, port := unixBootstrapSSHFields("root"); user != "root" || port != 22 {
		t.Fatalf("SSH fields = %q/%d", user, port)
	}
}
