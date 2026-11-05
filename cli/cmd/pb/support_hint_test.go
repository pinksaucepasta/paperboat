package main

import "testing"

func TestClientOnlyFailureSaysHowToSendItsDetails(t *testing.T) {
	const ref = "support_598bfe61-4113-4390-b17c-a7a0c0bc1858"
	for _, tc := range []struct {
		name, message string
		server        bool
		want          string
	}{
		{"client-only failure", "Paperboat could not finish: installing failed", false,
			"Paperboat could not finish: installing failed. Support reference: " + ref + ". Run `pb bugreport --upload` to send the details."},
		{"server-carried reference", "The server refused the request", true,
			"The server refused the request. Support reference: " + ref + "."},
		{"message already names it", "Failed (" + ref + ")", false, "Failed (" + ref + ")"},
		{"no reference", "Stopped.", false, "Stopped."},
	} {
		reference := ref
		if tc.name == "no reference" {
			reference = ""
		}
		if got := withSupportReference(tc.message, reference, tc.server); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}
