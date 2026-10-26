// Package compat emits every signed artifact the server sends to released clients, built by the
// production code, so tools/compat can decode them with the oldest supported official release's
// own verifiers (PLAN 3b rollout; the reverted f5628bae/8385e540 broke released daemons).
package compat
