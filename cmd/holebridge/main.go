// Command holebridge is the host and relay command line. The subcommands are added by later tasks.
package main

import (
	"os"
	"time"

	"github.com/andrewloable/HoleBridge/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], cli.Env{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Getenv: os.Getenv,
		Now:    time.Now,
	}))
}
