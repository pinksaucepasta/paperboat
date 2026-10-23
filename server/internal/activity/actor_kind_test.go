package activity

import "testing"

// Events whose actor was a deleted account show as "deleted user", never as Paperboat (system).
func TestActorKindNamesDeletedUsers(t *testing.T) {
	for _, c := range []struct{ actorType, token, want string }{
		{"user", "", "user"}, {"user", "tok_1", "api_token"}, {"directory", "", "directory"},
		{"admin", "", "operator"}, {"deleted_user", "", "deleted_user"}, {"system", "", "system"}, {"", "", "system"},
	} {
		if got := actorKind(c.actorType, c.token); got != c.want {
			t.Fatalf("actorKind(%q,%q) = %q, want %q", c.actorType, c.token, got, c.want)
		}
	}
}
