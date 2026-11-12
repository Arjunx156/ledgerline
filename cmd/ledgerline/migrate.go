package main

import (
	"fmt"
	"io"
	"strconv"

	"github.com/Arjunx156/ledgerline/internal/database"
)

func migrateCmd(args []string, getenv func(string) string, stdout io.Writer) error {
	if len(args) == 0 {
		return errUsage{"migrate needs up, down or version"}
	}
	var apply func(url string) error
	switch {
	case args[0] == "up" && len(args) == 1:
		apply = database.MigrateUp
	case args[0] == "down" && len(args) == 2 && args[1] == "all":
		apply = database.MigrateDownAll
	case args[0] == "down" && len(args) == 2:
		steps, err := strconv.Atoi(args[1])
		if err != nil || steps < 1 {
			return errUsage{fmt.Sprintf("migrate down needs a positive number or all, got %q", args[1])}
		}
		apply = func(url string) error { return database.MigrateDown(url, steps) }
	case args[0] == "version" && len(args) == 1:
		apply = func(string) error { return nil }
	case args[0] == "down":
		return errUsage{"migrate down needs a number of steps or all"}
	default:
		return errUsage{fmt.Sprintf("unknown migrate command %q", args[0])}
	}

	url, err := requireEnv(getenv, "LEDGERLINE_MIGRATE_URL")
	if err != nil {
		return err
	}
	if err := apply(url); err != nil {
		return err
	}
	version, dirty, err := database.MigrationVersion(url)
	if err != nil {
		return err
	}
	state := "clean"
	if dirty {
		state = "dirty: a migration failed partway; fix it, then force the version"
	}
	fmt.Fprintf(stdout, "migration version %d (%s)\n", version, state)
	return nil
}
