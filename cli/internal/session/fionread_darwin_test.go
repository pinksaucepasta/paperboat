//go:build darwin

package session

// fionread asks a pipe how many bytes are unread: FIONREAD, _IOR('f', 127, int)
// in <sys/filio.h>; golang.org/x/sys/unix does not export it for darwin.
const fionread = 0x4004667f
