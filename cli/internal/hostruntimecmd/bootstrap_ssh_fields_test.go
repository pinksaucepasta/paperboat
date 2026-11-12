package hostruntimecmd

import "testing"

// X-28: an OS account managed SSH cannot target is enrolled without a managed SSH target instead of failing.
func TestBootstrapSSHFieldsSkipsAnAccountManagedSSHCannotTarget(t *testing.T) {
	for _, servable := range []string{"runneradmin", `HOST\runneradmin`, "pb.user_1"} {
		if user, port := bootstrapSSHFields(" "+servable+" ", 22); user != servable || port != 22 {
			t.Errorf("%q: got %q %d", servable, user, port)
		}
	}
	for _, unservable := range []string{"Zoë Test", "a b", "zoë"} {
		if user, port := bootstrapSSHFields(unservable, 22); user != "" || port != 0 {
			t.Errorf("%q kept a managed SSH target: %q %d", unservable, user, port)
		}
	}
}
