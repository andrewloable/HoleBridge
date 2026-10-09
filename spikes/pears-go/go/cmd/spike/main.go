// Command spike runs the pears-go feasibility spike steps (throwaway, see docs/spike-m1.md).
//
//	spike dht -bootstrap 127.0.0.1:PORT [-js-ping HEX -js-find HEX]
//	spike noise -node path/to/noise-peer.js
//	spike udx -bare path/to/bare -peer path/to/udx-peer.js
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: spike dht|noise|udx [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "dht":
		err = runDHT(os.Args[2:])
	case "noise":
		err = runNoise(os.Args[2:])
	case "udx":
		err = runUDX(os.Args[2:])
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "spike:", err)
		os.Exit(1)
	}
}
