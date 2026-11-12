// Command ledgerline runs the Ledgerline payments API. Later stages add the
// worker, demo, seed and verify modes.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Arjunx156/ledgerline/internal/buildinfo"
)

const usage = `usage: ledgerline <command>

commands:
  serve                 run the API (LEDGERLINE_DATABASE_URL, LEDGERLINE_ADDR)
  migrate up            apply pending migrations (LEDGERLINE_MIGRATE_URL)
  migrate down <n|all>  roll back n migrations, or all of them
  migrate version       print the applied migration version
  version               print the commit this binary was built from
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// errUsage marks a mistake in how the command was called, as opposed to a
// failure while running it, so the exit code can tell them apart.
type errUsage struct{ msg string }

func (e errUsage) Error() string { return e.msg }

// run is main without the process exit, so tests can drive it.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "version":
		if len(args) != 1 {
			err = errUsage{"version takes no arguments"}
			break
		}
		fmt.Fprintf(stdout, "ledgerline %s\n", buildinfo.Read())
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
	case "serve":
		if len(args) != 1 {
			err = errUsage{"serve takes no arguments"}
			break
		}
		err = serve(ctx, getenv, slog.New(slog.NewJSONHandler(stdout, nil)))
	case "migrate":
		err = migrateCmd(args[1:], getenv, stdout)
	default:
		err = errUsage{fmt.Sprintf("unknown command %q", args[0])}
	}

	switch err.(type) {
	case nil:
		return 0
	case errUsage:
		fmt.Fprintf(stderr, "ledgerline: %v\n\n%s", err, usage)
		return 2
	default:
		fmt.Fprintf(stderr, "ledgerline: %v\n", err)
		return 1
	}
}

// requireEnv reads a setting that has no safe default.
func requireEnv(getenv func(string) string, name string) (string, error) {
	if v := getenv(name); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("%s is not set (for local development, copy .env.example to .env)", name)
}
