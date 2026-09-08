// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package magicsock

import (
	"errors"
	"testing"

	"tailscale.com/tailcfg"
)

func TestDERPTerminalErrorDoesNotFenceReplacement(t *testing.T) {
	c := newConn(t.Logf)
	old, replacement := &rebindTestCarrier{}, &rebindTestCarrier{}
	denied := errors.New("authority denied")
	c.activeDerp = map[tailcfg.DERPRegionID]activeDerp{1: {c: old}}
	c.recordDERPTerminalError(1, old, denied)
	if !errors.Is(c.activeDerp[1].terminalErr, denied) {
		t.Fatal("reader authority error lost")
	}
	c.activeDerp[1] = activeDerp{c: replacement}
	c.recordDERPTerminalError(1, old, denied)
	if c.activeDerp[1].terminalErr != nil {
		t.Fatal("late old reader fenced replacement")
	}
	delete(c.activeDerp, 1)
	c.recordDERPTerminalError(1, replacement, denied)
	if len(c.activeDerp) != 0 {
		t.Fatal("late reader recreated removed carrier")
	}
}
