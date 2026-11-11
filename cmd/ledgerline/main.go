// Command ledgerline runs the Ledgerline payments API. Later stages add the
// serve, worker, demo, seed and verify modes alongside version.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/Arjunx156/ledgerline/internal/buildinfo"
)

const usage = `usage: ledgerline <command>

commands:
  version   print the commit this binary was built from
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main without the process exit, so tests can drive it.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "ledgerline %s\n", buildinfo.Read())
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "ledgerline: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
