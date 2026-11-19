// Command loadsim is the load generator of the edge and relay capacity runs: it holds simulated machines on a real edge and
// drives HTTP through them. It reuses the agent's own carrier, hub and origin forwarder code, so an edge sees real carriers.
//
//	loadsim machines -dir D -control URL -machines N -routes R ...   hold N carriers with R endpoint routes each
//	loadsim http -edge ADDR -hosts N -mode small|large ...           request load through the routes
//	loadsim relay ...                                                relay load (relay.go)
//
// A development tool for the capacity runs (see tools/loadtest/README.md); never installed.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: loadsim machines|http|relay [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "machines":
		err = machines(os.Args[2:])
	case "http":
		err = httpLoad(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadsim:", err)
		os.Exit(1)
	}
}
