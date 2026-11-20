package supportbundle

import "testing"

func TestRedactStringRules(t *testing.T) {
	for _, test := range []struct {
		in, want string
		count    int
	}{
		{"Authorization: Bearer abc.def", "Authorization: " + RedactedValue, 2}, // header rule, then the key: value rule matches the already redacted line
		{`{"password": "hunter2"}`, `{"password": "` + RedactedValue + `"}`, 1},
		{"opened /home/user/.config/pb failed", "opened " + RedactedValue + " failed", 1},
		{"see https://user:pw@example.test/x", "see " + RedactedValue, 2},
		{"nothing sensitive here", "nothing sensitive here", 0},
	} {
		if got, count := redactString(test.in); got != test.want || count != test.count {
			t.Errorf("redactString(%q) = %q, %d; want %q, %d", test.in, got, count, test.want, test.count)
		}
	}
}
