// Package database opens Postgres pools and applies the embedded migrations.
package database

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the pgx5:// scheme
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Arjunx156/ledgerline/migrations"
)

// Open connects a pool and pings it, so a bad URL fails at startup rather
// than on the first request.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return pool, nil
}

// MigrateUp applies every pending migration. url must be the owner role's.
func MigrateUp(url string) error {
	return withMigrate(url, func(m *migrate.Migrate) error { return m.Up() })
}

// MigrateDown rolls back the given number of migrations.
func MigrateDown(url string, steps int) error {
	if steps < 1 {
		return fmt.Errorf("steps must be at least 1, got %d", steps)
	}
	return withMigrate(url, func(m *migrate.Migrate) error { return m.Steps(-steps) })
}

// MigrateDownAll rolls back every migration.
func MigrateDownAll(url string) error {
	return withMigrate(url, func(m *migrate.Migrate) error { return m.Down() })
}

// MigrationVersion reports the applied version, 0 when none is.
func MigrationVersion(url string) (version uint, dirty bool, err error) {
	err = withMigrate(url, func(m *migrate.Migrate) error {
		version, dirty, err = m.Version()
		if errors.Is(err, migrate.ErrNilVersion) {
			return nil
		}
		return err
	})
	return version, dirty, err
}

func withMigrate(url string, run func(*migrate.Migrate) error) error {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, pgx5URL(url))
	if err != nil {
		return fmt.Errorf("open migrations: %w", err)
	}
	runErr := run(m)
	srcErr, dbErr := m.Close()
	if runErr != nil && !errors.Is(runErr, migrate.ErrNoChange) {
		return fmt.Errorf("migrate: %w", runErr)
	}
	return errors.Join(srcErr, dbErr)
}

// golang-migrate picks its driver by scheme; the rest of the app uses the
// standard postgres:// form.
func pgx5URL(url string) string {
	for _, scheme := range []string{"postgres://", "postgresql://"} {
		if rest, ok := strings.CutPrefix(url, scheme); ok {
			return "pgx5://" + rest
		}
	}
	return url
}
