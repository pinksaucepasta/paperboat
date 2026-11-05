package main

import "strings"

// withSupportReference ends a failure line with its support reference. A reference that no server answer
// carried exists only on this computer (an install, a failure before any request), so the line also says how
// to send its details; a server-carried reference is already recorded on the server.
func withSupportReference(message, reference string, serverCarried bool) string {
	if reference == "" || strings.Contains(message, reference) {
		return message
	}
	message = sentence(message) + " Support reference: " + reference + "."
	if !serverCarried {
		message += " Run `pb bugreport --upload` to send the details."
	}
	return message
}
